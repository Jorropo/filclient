package filclient

import (
	"context"
	"fmt"
	"path/filepath"
	"strings"
	"time"

	"github.com/filecoin-project/go-address"
	cario "github.com/filecoin-project/go-commp-utils/pieceio/cario"
	datatransfer "github.com/filecoin-project/go-data-transfer"
	"github.com/filecoin-project/go-data-transfer/channelmonitor"
	dtimpl "github.com/filecoin-project/go-data-transfer/impl"
	dtnet "github.com/filecoin-project/go-data-transfer/network"
	gst "github.com/filecoin-project/go-data-transfer/transport/graphsync"
	commcid "github.com/filecoin-project/go-fil-commcid"
	commp "github.com/filecoin-project/go-fil-commp-hashhash"
	"github.com/filecoin-project/go-fil-markets/retrievalmarket"
	"github.com/filecoin-project/go-fil-markets/shared"
	"github.com/filecoin-project/go-fil-markets/storagemarket"
	"github.com/filecoin-project/go-fil-markets/storagemarket/impl/clientutils"
	"github.com/filecoin-project/go-fil-markets/storagemarket/impl/requestvalidation"
	"github.com/filecoin-project/go-fil-markets/storagemarket/network"
	"github.com/filecoin-project/go-state-types/abi"
	"github.com/filecoin-project/go-state-types/big"
	"github.com/filecoin-project/lotus/api"
	"github.com/filecoin-project/lotus/chain/actors/builtin/paych"
	rpcstmgr "github.com/filecoin-project/lotus/chain/stmgr/rpc"
	"github.com/filecoin-project/lotus/chain/types"
	"github.com/filecoin-project/lotus/chain/wallet"
	paychmgr "github.com/filecoin-project/lotus/paychmgr"
	"github.com/filecoin-project/specs-actors/actors/builtin"
	"github.com/filecoin-project/specs-actors/actors/builtin/market"
	"github.com/ipfs/go-cid"
	"github.com/ipfs/go-datastore"
	"github.com/ipfs/go-datastore/namespace"
	graphsync "github.com/ipfs/go-graphsync/impl"
	gsnet "github.com/ipfs/go-graphsync/network"
	storeutil "github.com/ipfs/go-graphsync/storeutil"
	blockstore "github.com/ipfs/go-ipfs-blockstore"
	logging "github.com/ipfs/go-log"
	"github.com/libp2p/go-libp2p-core/host"
	inet "github.com/libp2p/go-libp2p-core/network"
	"github.com/libp2p/go-libp2p-core/peer"
	protocol "github.com/libp2p/go-libp2p-protocol"
	"github.com/multiformats/go-multiaddr"
	"github.com/multiformats/go-multihash"
	"golang.org/x/xerrors"

	cborutil "github.com/filecoin-project/go-cbor-util"
)

var log = logging.Logger("filclient")

const DealProtocol = "/fil/storage/mk/1.1.0"
const QueryAskProtocol = "/fil/storage/ask/1.1.0"
const DealStatusProtocol = "/fil/storage/status/1.1.0"
const RetrievalQueryProtocol = "/fil/retrieval/qry/1.0.0"

type FilClient struct {
	mpusher *MsgPusher

	pchmgr *paychmgr.Manager

	host host.Host

	api api.Gateway

	wallet *wallet.LocalWallet

	clientAddr address.Address

	blockstore blockstore.Blockstore

	dataTransfer datatransfer.Manager

	computePieceComm GetPieceCommFunc
}

type GetPieceCommFunc func(rt abi.RegisteredSealProof, payloadCid cid.Cid, bstore blockstore.Blockstore) (cid.Cid, abi.UnpaddedPieceSize, error)

func NewClient(h host.Host, api api.Gateway, w *wallet.LocalWallet, addr address.Address, bs blockstore.Blockstore, ds datastore.Batching, ddir string) (*FilClient, error) {
	ctx, shutdown := context.WithCancel(context.Background())

	mpusher := NewMsgPusher(api, w)

	smapi := rpcstmgr.NewRPCStateManager(api)

	pchds := namespace.Wrap(ds, datastore.NewKey("paych"))
	store := paychmgr.NewStore(pchds)

	papi := &paychApiProvider{
		Gateway: api,
		wallet:  w,
		mp:      mpusher,
	}

	pchmgr := paychmgr.NewManager(ctx, shutdown, smapi, store, papi)
	if err := pchmgr.Start(); err != nil {
		return nil, err
	}

	gse := graphsync.New(context.Background(), gsnet.NewFromLibp2pHost(h), storeutil.LoaderForBlockstore(bs), storeutil.StorerForBlockstore(bs))
	tpt := gst.NewTransport(h.ID(), gse)
	dtn := dtnet.NewFromLibp2pHost(h)

	dtRestartConfig := dtimpl.ChannelRestartConfig(channelmonitor.Config{
		MonitorPushChannels:    true,
		AcceptTimeout:          time.Second * 30,
		Interval:               time.Minute,
		MinBytesTransferred:    4 << 10,
		ChecksPerInterval:      20,
		RestartBackoff:         time.Second * 20,
		MaxConsecutiveRestarts: 10,
		CompleteTimeout:        time.Second * 30,
	})
	mgr, err := dtimpl.NewDataTransfer(ds, filepath.Join(ddir, "cidlistsdir"), dtn, tpt, dtRestartConfig)
	if err != nil {
		return nil, err
	}

	err = mgr.RegisterVoucherType(&requestvalidation.StorageDataTransferVoucher{}, nil)
	if err != nil {
		return nil, err
	}

	err = mgr.RegisterVoucherType(&retrievalmarket.DealProposal{}, nil)
	if err != nil {
		return nil, err
	}

	err = mgr.RegisterVoucherType(&retrievalmarket.DealPayment{}, nil)
	if err != nil {
		return nil, err
	}

	err = mgr.RegisterVoucherResultType(&retrievalmarket.DealResponse{})
	if err != nil {
		return nil, err
	}

	if err := mgr.Start(context.TODO()); err != nil {
		return nil, err
	}

	return &FilClient{
		host:         h,
		api:          api,
		wallet:       w,
		clientAddr:   addr,
		blockstore:   bs,
		dataTransfer: mgr,
		pchmgr:       pchmgr,
		mpusher:      mpusher,
	}, nil
}

func (fc *FilClient) SetPieceCommFunc(pcf GetPieceCommFunc) {
	fc.computePieceComm = pcf
}

func (fc *FilClient) streamToMiner(ctx context.Context, maddr address.Address, protocol protocol.ID) (inet.Stream, error) {
	mpid, err := fc.connectToMiner(ctx, maddr)
	if err != nil {
		return nil, err
	}

	s, err := fc.host.NewStream(ctx, mpid, protocol)
	if err != nil {
		return nil, xerrors.Errorf("failed to open stream to peer: %w", err)
	}

	return s, nil
}

type ApiError struct {
	Under error
}

func (e *ApiError) Error() string {
	return fmt.Sprintf("api error: %s", e.Under)
}

func (e *ApiError) Unwrap() error {
	return e.Under
}

func (fc *FilClient) connectToMiner(ctx context.Context, maddr address.Address) (peer.ID, error) {
	minfo, err := fc.api.StateMinerInfo(ctx, maddr, types.EmptyTSK)
	if err != nil {
		return "", &ApiError{err}
	}

	if minfo.PeerId == nil {
		return "", fmt.Errorf("miner %s has no peer ID set", maddr)
	}

	var maddrs []multiaddr.Multiaddr
	for _, mma := range minfo.Multiaddrs {
		ma, err := multiaddr.NewMultiaddrBytes(mma)
		if err != nil {
			return "", fmt.Errorf("miner %s had invalid multiaddrs in their info: %w", maddr, err)
		}
		maddrs = append(maddrs, ma)
	}

	if err := fc.host.Connect(ctx, peer.AddrInfo{
		ID:    *minfo.PeerId,
		Addrs: maddrs,
	}); err != nil {
		return "", err
	}

	return *minfo.PeerId, nil
}

func (fc *FilClient) GetAsk(ctx context.Context, maddr address.Address) (*network.AskResponse, error) {
	s, err := fc.streamToMiner(ctx, maddr, QueryAskProtocol)
	if err != nil {
		return nil, err
	}
	defer s.Close()

	areq := &network.AskRequest{maddr}

	if err := cborutil.WriteCborRPC(s, areq); err != nil {
		return nil, xerrors.Errorf("failed to send query ask request: %w", err)
	}

	var resp network.AskResponse
	if err := cborutil.ReadCborRPC(s, &resp); err != nil {
		return nil, xerrors.Errorf("failed to read query response: %w", err)
	}

	return &resp, nil
}

const epochsPerHour = 60 * 2

func ComputePrice(askPrice types.BigInt, size abi.PaddedPieceSize, duration abi.ChainEpoch) (*abi.TokenAmount, error) {
	cost := big.Mul(big.Div(big.Mul(big.NewInt(int64(size)), askPrice), big.NewInt(1<<30)), big.NewInt(int64(duration)))

	return (*abi.TokenAmount)(&cost), nil
}

func (fc *FilClient) MakeDeal(ctx context.Context, miner address.Address, data cid.Cid, price types.BigInt, minSize abi.PaddedPieceSize, duration abi.ChainEpoch) (*network.Proposal, error) {
	sealType := abi.RegisteredSealProof_StackedDrg32GiBV1_1 // pull from miner...

	commP, size, err := fc.computePieceComm(sealType, data, fc.blockstore)
	if err != nil {
		return nil, err
	}

	if size.Padded() < minSize {
		padded, err := ZeroPadPieceCommitment(commP, size, minSize.Unpadded())
		if err != nil {
			return nil, err
		}

		commP = padded
		size = minSize.Unpadded()
	}

	fmt.Println("commp: ", commP)

	head, err := fc.api.ChainHead(ctx)
	if err != nil {
		return nil, err
	}

	verified := false
	collBounds, err := fc.api.StateDealProviderCollateralBounds(ctx, size.Padded(), verified, types.EmptyTSK)
	if err != nil {
		return nil, err
	}

	// set provider collateral 10% above minimum to avoid fluctuations causing deal failure
	provCol := big.Div(big.Mul(collBounds.Min, big.NewInt(11)), big.NewInt(10))

	dealStart := head.Height() + (epochsPerHour * 50)

	end := dealStart + duration

	pricePerEpoch := big.Div(big.Mul(big.NewInt(int64(size.Padded())), price), big.NewInt(1<<30))

	label, err := clientutils.LabelField(data)
	if err != nil {
		return nil, xerrors.Errorf("failed to construct label field: %w", err)
	}

	proposal := &market.DealProposal{
		PieceCID:     commP,
		PieceSize:    size.Padded(),
		VerifiedDeal: verified,
		Client:       fc.clientAddr,
		Provider:     miner,

		Label: label,

		StartEpoch: dealStart,
		EndEpoch:   end,

		StoragePricePerEpoch: pricePerEpoch,
		ProviderCollateral:   provCol,
		ClientCollateral:     big.Zero(),
	}

	raw, err := cborutil.Dump(proposal)
	if err != nil {
		return nil, err
	}
	sig, err := fc.wallet.WalletSign(ctx, fc.clientAddr, raw, api.MsgMeta{Type: api.MTDealProposal})
	if err != nil {
		return nil, err
	}

	sigprop := &market.ClientDealProposal{
		Proposal:        *proposal,
		ClientSignature: *sig,
	}

	return &network.Proposal{
		DealProposal: sigprop,
		Piece: &storagemarket.DataRef{
			TransferType: storagemarket.TTGraphsync,
			Root:         data,
		},
		FastRetrieval: true,
	}, nil
}

func (fc *FilClient) SendProposal(ctx context.Context, netprop *network.Proposal) (*network.SignedResponse, error) {
	s, err := fc.streamToMiner(ctx, netprop.DealProposal.Proposal.Provider, DealProtocol)
	if err != nil {
		return nil, xerrors.Errorf("opening stream to miner: %w", err)
	}

	defer s.Close()

	if err := cborutil.WriteCborRPC(s, netprop); err != nil {
		return nil, xerrors.Errorf("failed to write proposal to miner: %w", err)
	}

	var resp network.SignedResponse
	if err := cborutil.ReadCborRPC(s, &resp); err != nil {
		return nil, xerrors.Errorf("failed to read response from miner: %w", err)
	}

	return &resp, nil
}

func GeneratePieceCommitment(rt abi.RegisteredSealProof, payloadCid cid.Cid, bstore blockstore.Blockstore) (cid.Cid, abi.UnpaddedPieceSize, error) {
	cario := cario.NewCarIO()
	preparedCar, err := cario.PrepareCar(context.Background(), bstore, payloadCid, shared.AllSelector())
	if err != nil {
		return cid.Undef, 0, err
	}

	cp := new(commp.Calc)
	err = preparedCar.Dump(cp)
	if err != nil {
		return cid.Undef, 0, err
	}

	rawCommP, paddedSize, err := cp.Digest()
	if err != nil {
		return cid.Undef, 0, err
	}

	commCid, err := commcid.DataCommitmentV1ToCID(rawCommP)
	if err != nil {
		return cid.Undef, 0, err
	}

	return commCid, paddedSize.Unpadded(), nil
}

func ZeroPadPieceCommitment(c cid.Cid, curSize abi.UnpaddedPieceSize, toSize abi.UnpaddedPieceSize) (cid.Cid, error) {
	dmh, err := multihash.Decode(c.Hash())
	if err != nil {
		return cid.Undef, err
	}

	rawCommP, err := commp.PadCommP(dmh.Digest, curSize.Padded(), toSize.Padded())
	if err != nil {
		return cid.Undef, err
	}

	return commcid.DataCommitmentV1ToCID(rawCommP)

	/*
		cur := c
		for curSize < toSize {

			zc := zerocomm.ZeroPieceCommitment(curSize)

			p, err := ffi.GenerateUnsealedCID(abi.RegisteredSealProof_StackedDrg32GiBV1, []abi.PieceInfo{
				abi.PieceInfo{
					Size:     curSize.Padded(),
					PieceCID: cur,
				},
				abi.PieceInfo{
					Size:     curSize.Padded(),
					PieceCID: zc,
				},
			})
			if err != nil {
				return cid.Undef, err
			}

			cur = p
			curSize = curSize * 2
		}

		return cur, nil
	*/
}

func (fc *FilClient) DealStatus(ctx context.Context, miner address.Address, propCid cid.Cid) (*storagemarket.ProviderDealState, error) {
	cidb, err := cborutil.Dump(propCid)
	if err != nil {
		return nil, err
	}

	sig, err := fc.wallet.WalletSign(ctx, fc.clientAddr, cidb, api.MsgMeta{Type: api.MTUnknown})
	if err != nil {
		return nil, xerrors.Errorf("signing status request failed: %w", err)
	}

	req := &network.DealStatusRequest{
		Proposal:  propCid,
		Signature: *sig,
	}

	s, err := fc.streamToMiner(ctx, miner, DealStatusProtocol)
	if err != nil {
		return nil, err
	}

	if err := cborutil.WriteCborRPC(s, req); err != nil {
		return nil, xerrors.Errorf("failed to write status request: %w", err)
	}

	var resp network.DealStatusResponse
	if err := cborutil.ReadCborRPC(s, &resp); err != nil {
		return nil, xerrors.Errorf("reading response: %w", err)
	}

	// TODO: check the signatures and stuff?

	return &resp.DealState, nil
}

func (fc *FilClient) minerPeer(ctx context.Context, miner address.Address) (peer.ID, error) {
	minfo, err := fc.api.StateMinerInfo(ctx, miner, types.EmptyTSK)
	if err != nil {
		return "", err
	}
	if minfo.PeerId == nil {
		return "", fmt.Errorf("miner has no peer id")
	}

	return *minfo.PeerId, nil
}

func (fc *FilClient) minerOwner(ctx context.Context, miner address.Address) (address.Address, error) {
	minfo, err := fc.api.StateMinerInfo(ctx, miner, types.EmptyTSK)
	if err != nil {
		return address.Undef, err
	}
	if minfo.PeerId == nil {
		return address.Undef, fmt.Errorf("miner has no peer id")
	}

	return minfo.Owner, nil
}

type ChannelState struct {
	//datatransfer.Channel

	// SelfPeer returns the peer this channel belongs to
	SelfPeer   peer.ID `json:"selfPeer"`
	RemotePeer peer.ID `json:"remotePeer"`

	// Status is the current status of this channel
	Status    datatransfer.Status `json:"status"`
	StatusStr string              `json:"statusMessage"`

	// Sent returns the number of bytes sent
	Sent uint64 `json:"sent"`

	// Received returns the number of bytes received
	Received uint64 `json:"received"`

	// Message offers additional information about the current status
	Message string `json:"message"`

	BaseCid string `json:"baseCid"`

	ChannelID datatransfer.ChannelID `json:"channelId"`

	// Vouchers returns all vouchers sent on this channel
	//Vouchers []datatransfer.Voucher

	// VoucherResults are results of vouchers sent on the channel
	//VoucherResults []datatransfer.VoucherResult

	// LastVoucher returns the last voucher sent on the channel
	//LastVoucher datatransfer.Voucher

	// LastVoucherResult returns the last voucher result sent on the channel
	//LastVoucherResult datatransfer.VoucherResult

	// ReceivedCids returns the cids received so far on the channel
	//ReceivedCids []cid.Cid

	// Queued returns the number of bytes read from the node and queued for sending
	//Queued uint64
}

func ChannelStateConv(st datatransfer.ChannelState) *ChannelState {
	return &ChannelState{
		SelfPeer:   st.SelfPeer(),
		RemotePeer: st.OtherPeer(),
		Status:     st.Status(),
		StatusStr:  datatransfer.Statuses[st.Status()],
		Sent:       st.Sent(),
		Received:   st.Received(),
		Message:    st.Message(),
		BaseCid:    st.BaseCID().String(),
		ChannelID:  st.ChannelID(),
		//Vouchers:          st.Vouchers(),
		//VoucherResults:    st.VoucherResults(),
		//LastVoucher:       st.LastVoucher(),
		//LastVoucherResult: st.LastVoucherResult(),
		//ReceivedCids:      st.ReceivedCids(),
		//Queued:            st.Queued(),
	}
}

func (fc *FilClient) TransfersInProgress(ctx context.Context) (map[datatransfer.ChannelID]datatransfer.ChannelState, error) {
	return fc.dataTransfer.InProgressChannels(ctx)
}

func (fc *FilClient) TransferStatus(ctx context.Context, chanid *datatransfer.ChannelID) (*ChannelState, error) {
	st, err := fc.dataTransfer.ChannelState(ctx, *chanid)
	if err != nil {
		return nil, err
	}

	return ChannelStateConv(st), nil
}

var ErrNoTransferFound = fmt.Errorf("no transfer found")

func (fc *FilClient) TransferStatusForContent(ctx context.Context, content cid.Cid, miner address.Address) (*ChannelState, error) {
	start := time.Now()
	defer func() {
		log.Infof("check transfer status took: %s", time.Since(start))
	}()
	mpid, err := fc.minerPeer(ctx, miner)
	if err != nil {
		return nil, err
	}

	inprog, err := fc.dataTransfer.InProgressChannels(ctx)
	if err != nil {
		return nil, err
	}

	for chanid, state := range inprog {
		if chanid.Responder == mpid {
			if state.IsPull() {
				// this isnt a storage deal transfer...
				continue
			}
			if state.BaseCID() == content {
				return ChannelStateConv(state), nil
			}
		}
	}

	return nil, ErrNoTransferFound
}

func (fc *FilClient) RestartTransfer(ctx context.Context, chanid *datatransfer.ChannelID) error {
	return fc.dataTransfer.RestartDataTransferChannel(ctx, *chanid)
}

func (fc *FilClient) StartDataTransfer(ctx context.Context, miner address.Address, propCid cid.Cid, dataCid cid.Cid) (*datatransfer.ChannelID, error) {
	mpid, err := fc.minerPeer(ctx, miner)
	if err != nil {
		return nil, xerrors.Errorf("getting miner peer: %w", err)
	}

	voucher := &requestvalidation.StorageDataTransferVoucher{Proposal: propCid}

	fc.host.ConnManager().Protect(mpid, "transferring")

	chanid, err := fc.dataTransfer.OpenPushDataChannel(ctx, mpid, voucher, dataCid, shared.AllSelector())
	if err != nil {
		return nil, xerrors.Errorf("opening push data channel: %w", err)
	}

	return &chanid, nil
}

type Balance struct {
	Account         address.Address `json:"account"`
	Balance         types.FIL       `json:"balance"`
	MarketEscrow    types.FIL       `json:"marketEscrow"`
	MarketLocked    types.FIL       `json:"marketLocked"`
	MarketAvailable types.FIL       `json:"marketAvailable"`
}

func (fc *FilClient) Balance(ctx context.Context) (*Balance, error) {
	act, err := fc.api.StateGetActor(ctx, fc.clientAddr, types.EmptyTSK)
	if err != nil {
		return nil, err
	}

	market, err := fc.api.StateMarketBalance(ctx, fc.clientAddr, types.EmptyTSK)
	if err != nil {
		return nil, err
	}

	avail := types.BigSub(market.Escrow, market.Locked)

	return &Balance{
		Account:         fc.clientAddr,
		Balance:         types.FIL(act.Balance),
		MarketEscrow:    types.FIL(market.Escrow),
		MarketLocked:    types.FIL(market.Locked),
		MarketAvailable: types.FIL(avail),
	}, nil
}

type LockFundsResp struct {
	MsgCid cid.Cid
}

func (fc *FilClient) LockMarketFunds(ctx context.Context, amt types.FIL) (*LockFundsResp, error) {

	act, err := fc.api.StateGetActor(ctx, fc.clientAddr, types.EmptyTSK)
	if err != nil {
		return nil, err
	}

	if types.BigCmp(types.BigInt(amt), act.Balance) > 0 {
		return nil, fmt.Errorf("not enough funds to add: %s < %s", types.FIL(act.Balance), amt)
	}

	encAddr, err := cborutil.Dump(&fc.clientAddr)
	if err != nil {
		return nil, err
	}

	msg := &types.Message{
		From:   fc.clientAddr,
		To:     builtin.StorageMarketActorAddr,
		Method: builtin.MethodsMarket.AddBalance,
		Value:  types.BigInt(amt),
		Params: encAddr,
		Nonce:  act.Nonce,
	}

	smsg, err := fc.mpusher.MpoolPushMessage(ctx, msg, &api.MessageSendSpec{})
	if err != nil {
		return nil, err
	}

	return &LockFundsResp{
		MsgCid: smsg.Cid(),
	}, nil
}

func (fc *FilClient) CheckChainDeal(ctx context.Context, dealid abi.DealID) (bool, error) {
	deal, err := fc.api.StateMarketStorageDeal(ctx, dealid, types.EmptyTSK)
	if err != nil {
		nfs := fmt.Sprintf("deal %d not found", dealid)
		if strings.Contains(err.Error(), nfs) {
			return false, nil
		}

		return false, err
	}

	if deal.State.SlashEpoch > 0 {
		return false, nil
	}

	return true, nil
}

func (fc *FilClient) CheckOngoingTransfer(ctx context.Context, miner address.Address, st *ChannelState) (outerr error) {
	defer func() {
		// TODO: this is only here because for some reason restarting a data transfer can just panic
		// https://github.com/filecoin-project/go-data-transfer/issues/150
		if e := recover(); e != nil {
			outerr = fmt.Errorf("panic while checking transfer: %s", e)
		}
	}()
	// make sure we at least have an open connection to the miner
	if fc.host.Network().Connectedness(st.RemotePeer) != inet.Connected {
		// try reconnecting
		mpid, err := fc.connectToMiner(ctx, miner)
		if err != nil {
			return xerrors.Errorf("failed to reconnect to miner: %w", err)
		}

		if mpid != st.RemotePeer {
			return fmt.Errorf("miner peer ID is different than RemotePeer in data transfer channel")
		}
	}

	return fc.dataTransfer.RestartDataTransferChannel(ctx, st.ChannelID)

}

func (fc *FilClient) RetrievalQuery(ctx context.Context, maddr address.Address, pcid cid.Cid) (*retrievalmarket.QueryResponse, error) {
	s, err := fc.streamToMiner(ctx, maddr, RetrievalQueryProtocol)
	if err != nil {
		return nil, err
	}

	q := &retrievalmarket.Query{
		PayloadCID: pcid,
	}

	if err := cborutil.WriteCborRPC(s, q); err != nil {
		return nil, err
	}

	var resp retrievalmarket.QueryResponse
	if err := cborutil.ReadCborRPC(s, &resp); err != nil {
		return nil, err
	}

	return &resp, nil
}

func (fc *FilClient) getPaychWithMinFunds(ctx context.Context, dest address.Address) (address.Address, error) {

	avail, err := fc.pchmgr.AvailableFundsByFromTo(fc.clientAddr, dest)
	if err != nil {
		return address.Undef, err
	}

	reqBalance, err := types.ParseFIL("0.01")
	if err != nil {
		return address.Undef, err
	}
	fmt.Println("available", avail.ConfirmedAmt)

	if types.BigCmp(avail.ConfirmedAmt, types.BigInt(reqBalance)) >= 0 {
		return *avail.Channel, nil
	}

	amount := abi.TokenAmount(types.BigMul(types.BigInt(reqBalance), types.NewInt(2)))

	fmt.Println("getting payment channel: ", fc.clientAddr, dest, amount)
	pchaddr, mcid, err := fc.pchmgr.GetPaych(ctx, fc.clientAddr, dest, amount)
	if err != nil {
		return address.Undef, xerrors.Errorf("failed to get payment channel: %w", err)
	}

	fmt.Println("got payment channel: ", pchaddr, mcid)
	if !mcid.Defined() {
		if pchaddr == address.Undef {
			return address.Undef, xerrors.Errorf("GetPaych returned nothing")
		}

		return pchaddr, nil
	}

	return fc.pchmgr.GetPaychWaitReady(ctx, mcid)
}

type RetrievalStats struct {
	Peer         peer.ID
	Size         uint64
	Duration     time.Duration
	AverageSpeed uint64
	TotalPayment abi.TokenAmount
	NumPayments  int
	AskPrice     abi.TokenAmount
}

func (fc *FilClient) RetrieveContent(ctx context.Context, miner address.Address, proposal *retrievalmarket.DealProposal) (*RetrievalStats, error) {
	start := time.Now()
	log.Infof("attempting retrieval with miner: %s", miner)
	mpid, err := fc.minerPeer(ctx, miner)
	if err != nil {
		return nil, xerrors.Errorf("failed to get miner peer: %w", err)
	}

	minerOwner, err := fc.minerOwner(ctx, miner)
	if err != nil {
		return nil, err
	}

	pchaddr, err := fc.getPaychWithMinFunds(ctx, minerOwner)
	if err != nil {
		return nil, xerrors.Errorf("failed to get payment channel: %w", err)
	}

	// Allocate a lane on our payment channel, usually you want a new lane per retrieval
	lane, err := fc.pchmgr.AllocateLane(pchaddr)
	if err != nil {
		return nil, xerrors.Errorf("failed to allocate lane: %w", err)
	}

	// Use the data transfer protocol to propose the retrieval deal
	sel := shared.AllSelector()
	var vouch datatransfer.Voucher = proposal
	chanid, err := fc.dataTransfer.OpenPullDataChannel(ctx, mpid, vouch, proposal.PayloadCID, sel)
	if err != nil {
		return nil, err
	}

	// NB: data transfer will propose the retrieval, and if the miner accepts
	// it, will start sending data via graphsync to us. This happens behind the
	// scenes, and we dont get much introspection into this

	// Now, we poll the data transfer channel status and wait until it says its accepted
	if err := fc.waitForDealAccepted(ctx, chanid); err != nil {
		return nil, err
	}

	var nonce uint64
	total := abi.NewTokenAmount(0)

	var voucherCount int
	for {
		// Now that the deal has been accepted, we sit around and wait for data
		// to come in and for the miner to ask for more money
		amt, nvc, done, err := fc.waitForPaymentNeeded(ctx, chanid, voucherCount)
		if err != nil {
			return nil, xerrors.Errorf("waitForPayment needed failed: %w", err)
		}

		if done {
			break
		}

		voucherCount = nvc

		total = types.BigAdd(total, *amt)

		vres, err := fc.pchmgr.CreateVoucher(ctx, pchaddr, paych.SignedVoucher{
			ChannelAddr: pchaddr,
			Lane:        lane,
			Nonce:       nonce,
			Amount:      total,
		})
		if err != nil {
			return nil, err
		}

		if types.BigCmp(vres.Shortfall, big.NewInt(0)) > 0 {
			return nil, fmt.Errorf("not enough funds remaining in payment channel (shortfall = %s)", vres.Shortfall)
		}

		paymnt := &retrievalmarket.DealPayment{
			ID:             proposal.ID,
			PaymentChannel: pchaddr,
			PaymentVoucher: vres.Voucher,
		}

		if err := fc.dataTransfer.SendVoucher(ctx, chanid, paymnt); err != nil {
			return nil, xerrors.Errorf("failed to send payment voucher: %w", err)
		}

		nonce++
	}

	st, err := fc.dataTransfer.ChannelState(ctx, chanid)
	if err != nil {
		return nil, err
	}

	took := time.Since(start)
	speed := uint64(float64(st.TotalSize()) / took.Seconds())

	return &RetrievalStats{
		Peer:         st.OtherPeer(),
		Size:         st.TotalSize(),
		Duration:     took,
		AverageSpeed: speed,
		TotalPayment: total,
		NumPayments:  int(nonce),
		AskPrice:     proposal.PricePerByte,
	}, nil
}

func (fc *FilClient) waitForDealAccepted(ctx context.Context, chanid datatransfer.ChannelID) error {
	// looping here is dumb, but the alternative is to write a lot of code that
	// connects into the data transfer events watcher thing and interprets the
	// entrails of our sacrifices to determine when to do the next things
	for {
		st, err := fc.dataTransfer.ChannelState(ctx, chanid)
		if err != nil {
			return err
		}

		results := st.VoucherResults()
		var res datatransfer.VoucherResult
		if len(results) > 0 {
			res = results[len(results)-1]
		}
		switch rest := res.(type) {
		case *retrievalmarket.DealResponse:
			switch rest.Status {
			case retrievalmarket.DealStatusAccepted:
				log.Infow("retrieval deal accepted! ", "msg", rest.Message, "owed", types.FIL(rest.PaymentOwed), "received", st.Received())
				return nil
			case retrievalmarket.DealStatusRejected:
				return fmt.Errorf("deal rejected: %s", rest.Message)
			case retrievalmarket.DealStatusFundsNeededUnseal:
				return fmt.Errorf("data needs to be unsealed")
			case retrievalmarket.DealStatusErrored:
				return fmt.Errorf("retrieval deal errored: %s", rest.Message)
			default:
				return fmt.Errorf("unexpected status while waiting for accept: %d", rest.Status)
			}
		default:
			if res != nil {
				return fmt.Errorf("unrecognized voucher response type: %T", res)
			}
		}

		time.Sleep(pollingDelay)
	}

	return nil
}

const noDataTimeout = time.Second * 20

const pollingDelay = time.Millisecond * 200

func (fc *FilClient) waitForPaymentNeeded(ctx context.Context, chanid datatransfer.ChannelID, curcount int) (*abi.TokenAmount, int, bool, error) {

	var lastReceived uint64
	lastReceivedTime := time.Now()

	for {
		st, err := fc.dataTransfer.ChannelState(ctx, chanid)
		if err != nil {
			return nil, 0, false, err
		}
		if lastReceived == st.Received() {
			if time.Since(lastReceivedTime) > noDataTimeout {
				return nil, 0, false, fmt.Errorf("timed out waiting for data or payment request")
			}
		} else {
			lastReceived = st.Received()
			lastReceivedTime = time.Now()
		}

		fmt.Println("retrieval progress: ", st.Received())

		switch st.Status() {
		case datatransfer.TransferFinished:
			return nil, 0, true, nil
		case datatransfer.ResponderPaused:
			// fmt.Println("for some reason the status is 'paused'")
		case datatransfer.Ongoing:
			// this is good?
		case datatransfer.ResponderFinalizingTransferFinished:
			// i think this is good.
			return nil, 0, true, nil
		default:
			return nil, 0, false, fmt.Errorf("unrecognized transfer status: %d", st.Status())

		}

		voucherResults := st.VoucherResults()
		if curcount == len(voucherResults) {
			// no new voucher results, dont want to make decisions based on old
			// information
			time.Sleep(pollingDelay)
			continue
		}

		res := voucherResults[len(voucherResults)-1]
		switch rest := res.(type) {
		case *retrievalmarket.DealResponse:
			switch rest.Status {
			case retrievalmarket.DealStatusAccepted:
				// this is what we want to see, means things are good for now
				//fmt.Println("Accepted status while waiting for data! ", rest.Message, types.FIL(rest.PaymentOwed), st.Received())
			case retrievalmarket.DealStatusFundsNeeded:
				return &rest.PaymentOwed, len(voucherResults), false, nil
			case retrievalmarket.DealStatusFundsNeededUnseal:
				log.Warnf("miner needs to unseal data, not retrieving from here")
				return nil, 0, false, fmt.Errorf("payment for unsealing requested unexpectedly")
			default:
				return nil, 0, false, fmt.Errorf("unexpected status while waiting for accept: %d", rest.Status)
			}
		default:
			if res != nil {
				return nil, 0, false, fmt.Errorf("unrecognized voucher response type: %T", res)
			}
		}

		time.Sleep(pollingDelay)
	}
}
