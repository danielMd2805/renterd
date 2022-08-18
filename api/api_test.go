package api_test

import (
	"bytes"
	"context"
	"io"
	"io/ioutil"
	"net"
	"net/http"
	"testing"

	"go.sia.tech/renterd/api"
	"go.sia.tech/renterd/internal/consensus"
	"go.sia.tech/renterd/internal/contractsutil"
	"go.sia.tech/renterd/internal/hostdbutil"
	"go.sia.tech/renterd/internal/objectutil"
	"go.sia.tech/renterd/internal/slabutil"
	"go.sia.tech/renterd/internal/walletutil"
	"go.sia.tech/renterd/object"
	rhpv2 "go.sia.tech/renterd/rhp/v2"
	rhpv3 "go.sia.tech/renterd/rhp/v3"
	"go.sia.tech/renterd/slab"
	"go.sia.tech/renterd/wallet"
	"go.sia.tech/siad/types"
	"lukechampine.com/frand"
)

type mockChainManager struct{}

func (mockChainManager) TipState() (cs consensus.State) { return }

type mockSyncer struct{}

func (mockSyncer) Addr() string              { return "" }
func (mockSyncer) Peers() []string           { return nil }
func (mockSyncer) Connect(addr string) error { return nil }
func (mockSyncer) BroadcastTransaction(txn types.Transaction, dependsOn []types.Transaction) {
}

type mockTxPool struct{}

func (mockTxPool) RecommendedFee() types.Currency                   { return types.ZeroCurrency }
func (mockTxPool) Transactions() []types.Transaction                { return nil }
func (mockTxPool) AddTransactionSet(txns []types.Transaction) error { return nil }
func (mockTxPool) UnconfirmedParents(txn types.Transaction) ([]types.Transaction, error) {
	return nil, nil
}

type mockRHP struct{}

func (mockRHP) Settings(ctx context.Context, hostIP string, hostKey consensus.PublicKey) (rhpv2.HostSettings, error) {
	return rhpv2.HostSettings{}, nil
}

func (mockRHP) FormContract(ctx context.Context, cs consensus.State, hostIP string, hostKey consensus.PublicKey, renterKey consensus.PrivateKey, txns []types.Transaction, walletKey consensus.PrivateKey) (rhpv2.Contract, []types.Transaction, error) {
	txn := txns[len(txns)-1]
	fc := txn.FileContracts[0]
	return rhpv2.Contract{
		Revision: types.FileContractRevision{
			ParentID: txn.FileContractID(0),
			UnlockConditions: types.UnlockConditions{
				PublicKeys: []types.SiaPublicKey{
					{Algorithm: types.SignatureEd25519, Key: renterKey[:]},
					{Algorithm: types.SignatureEd25519, Key: hostKey[:]},
				},
				SignaturesRequired: 2,
			},
			NewRevisionNumber:     1,
			NewFileSize:           fc.FileSize,
			NewFileMerkleRoot:     fc.FileMerkleRoot,
			NewWindowStart:        fc.WindowStart,
			NewWindowEnd:          fc.WindowEnd,
			NewValidProofOutputs:  fc.ValidProofOutputs,
			NewMissedProofOutputs: fc.MissedProofOutputs,
			NewUnlockHash:         fc.UnlockHash,
		},
	}, nil, nil
}

func (mockRHP) RenewContract(ctx context.Context, cs consensus.State, hostIP string, hostKey consensus.PublicKey, renterKey consensus.PrivateKey, contractID types.FileContractID, txns []types.Transaction, finalPayment types.Currency, walletKey consensus.PrivateKey) (rhpv2.Contract, []types.Transaction, error) {
	return rhpv2.Contract{}, nil, nil
}

func (mockRHP) FundAccount(ctx context.Context, hostIP string, hostKey consensus.PublicKey, contract types.FileContractRevision, renterKey consensus.PrivateKey, account rhpv3.Account, amount types.Currency) (rhpv2.Contract, error) {
	return rhpv2.Contract{}, nil
}

func (mockRHP) ReadRegistry(ctx context.Context, hostIP string, hostKey consensus.PublicKey, payment rhpv3.PaymentMethod, registryKey rhpv3.RegistryKey) (rhpv3.RegistryValue, error) {
	return rhpv3.RegistryValue{}, nil
}

func (mockRHP) UpdateRegistry(ctx context.Context, hostIP string, hostKey consensus.PublicKey, payment rhpv3.PaymentMethod, registryKey rhpv3.RegistryKey, registryValue rhpv3.RegistryValue) error {
	return nil
}

type mockSlabMover struct {
	hs *slabutil.MockHostSet
}

func (sm mockSlabMover) UploadSlabs(ctx context.Context, r io.Reader, m, n uint8, currentHeight uint64, contracts []api.Contract) ([]slab.Slab, error) {
	ssu := slab.SerialSlabsUploader{SlabUploader: slab.SerialSlabUploader{Hosts: sm.hs.Uploaders()}}
	return ssu.UploadSlabs(r, m, n)
}

func (sm mockSlabMover) DownloadSlabs(ctx context.Context, w io.Writer, slabs []slab.Slice, offset, length int64, contracts []api.Contract) error {
	ssd := slab.SerialSlabsDownloader{SlabDownloader: slab.SerialSlabDownloader{Hosts: sm.hs.Downloaders()}}
	return ssd.DownloadSlabs(w, slabs, offset, length)
}

func (sm mockSlabMover) DeleteSlabs(ctx context.Context, slabs []slab.Slab, contracts []api.Contract) error {
	ssd := slab.SerialSlabsDeleter{Hosts: sm.hs.Deleters()}
	return ssd.DeleteSlabs(slabs)
}

type node struct {
	w   *wallet.SingleAddressWallet
	hdb *hostdbutil.EphemeralDB
	cs  *contractsutil.EphemeralStore
	os  *objectutil.EphemeralStore
	sm  mockSlabMover

	walletKey consensus.PrivateKey
}

func (n *node) addHost() consensus.PublicKey {
	return n.sm.hs.AddHost()
}

func newTestNode() *node {
	walletKey := consensus.GeneratePrivateKey()
	w := wallet.NewSingleAddressWallet(walletKey, walletutil.NewEphemeralStore(wallet.StandardAddress(walletKey.PublicKey())))
	hdb := hostdbutil.NewEphemeralDB()
	cs := contractsutil.NewEphemeralStore()
	os := objectutil.NewEphemeralStore()
	sm := mockSlabMover{hs: slabutil.NewMockHostSet()}
	return &node{w, hdb, cs, os, sm, walletKey}
}

func runServer(n *node) (*api.Client, func()) {
	l, err := net.Listen("tcp", ":0")
	if err != nil {
		panic(err)
	}
	go func() {
		srv := api.NewServer(mockChainManager{}, mockSyncer{}, mockTxPool{}, n.w, n.hdb, mockRHP{}, n.cs, n.sm, n.os)
		http.Serve(l, api.AuthMiddleware(srv, "password"))
	}()
	c := api.NewClient("http://"+l.Addr().String(), "password")
	return c, func() { l.Close() }
}

func TestSlabs(t *testing.T) {
	n := newTestNode()
	c, shutdown := runServer(n)
	defer shutdown()

	hosts := make([]consensus.PublicKey, 3)
	for i := range hosts {
		hosts[i] = n.addHost()
	}

	// form contracts
	var contracts []api.Contract
	for _, hostKey := range hosts {
		const hostIP = ""
		settings, err := c.RHPScan(hostKey, hostIP)
		if err != nil {
			t.Fatal(err)
		}
		renterKey := consensus.GeneratePrivateKey()
		addr, _ := c.WalletAddress()
		fc, cost, err := c.RHPPrepareForm(renterKey, hostKey, types.ZeroCurrency, addr, types.ZeroCurrency, 0, settings)
		if err != nil {
			t.Fatal(err)
		}
		txn := types.Transaction{
			FileContracts: []types.FileContract{fc},
		}
		parents, err := c.WalletFund(&txn, cost)
		if err != nil {
			t.Fatal(err)
		}
		c, _, err := c.RHPForm(renterKey, hostKey, hostIP, append(parents, txn), n.walletKey)
		if err != nil {
			t.Fatal(err)
		}
		contracts = append(contracts, api.Contract{
			HostKey:   c.HostKey(),
			HostIP:    hostIP,
			ID:        c.ID(),
			RenterKey: renterKey,
		})
	}

	// upload
	data := frand.Bytes(20)
	key := object.GenerateEncryptionKey()
	slabs, err := c.UploadSlabs(key.Encrypt(bytes.NewReader(data)), 2, 3, contracts)
	if err != nil {
		t.Fatal(err)
	}
	o := object.Object{
		Key:   key,
		Slabs: make([]slab.Slice, len(slabs)),
	}
	for i := range slabs {
		o.Slabs[i] = slab.Slice{
			Slab:   slabs[i],
			Offset: 0,
			Length: uint32(len(data)),
		}
	}

	// store object
	if err := c.AddObject("foo", o); err != nil {
		t.Fatal(err)
	}

	// retrieve object
	o, err = c.Object("foo")
	if err != nil {
		t.Fatal(err)
	}

	// download
	var buf bytes.Buffer
	if err := c.DownloadSlabs(key.Decrypt(&buf, 0), o.Slabs, 0, o.Size(), contracts); err != nil {
		t.Fatal(err)
	} else if !bytes.Equal(buf.Bytes(), data) {
		t.Fatalf("data mismatch:\n%v (%v)\n%v (%v)", buf.Bytes(), len(buf.Bytes()), data, len(data))
	}

	// delete slabs
	if err := c.DeleteSlabs(slabs, contracts); err != nil {
		t.Fatal(err)
	}
	if err := c.DownloadSlabs(ioutil.Discard, o.Slabs, 0, o.Size(), contracts); err == nil {
		t.Error("slabs should no longer be retrievable")
	}

	// delete object
	if err := c.DeleteObject("foo"); err != nil {
		t.Fatal(err)
	}
	if _, err := c.Object("foo"); err == nil {
		t.Error("object should no longer be retrievable")
	}
}
