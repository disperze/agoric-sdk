package vtransfer_test

import (
	sdkmath "cosmossdk.io/math"
	sdk "github.com/cosmos/cosmos-sdk/types"
	"github.com/cosmos/cosmos-sdk/types/bech32"
	authtypes "github.com/cosmos/cosmos-sdk/x/auth/types"
	govtypes "github.com/cosmos/cosmos-sdk/x/gov/types"
	ibctransfertypes "github.com/cosmos/ibc-go/v10/modules/apps/transfer/types"
	channeltypes "github.com/cosmos/ibc-go/v10/modules/core/04-channel/types"
	ibctesting "github.com/cosmos/ibc-go/v10/testing"
	usdcmigrationkeeper "github.com/disperze/usdc-migration/v10/x/usdcmigration/keeper"
	usdcmigrationtypes "github.com/disperze/usdc-migration/v10/x/usdcmigration/types"
)

// sendUusdcFromNoble sends amount of uusdc from chainB (standing in for Noble)
// to chainA over path, relays the packet and returns chainA's acknowledgement.
func (s *IntegrationTestSuite) sendUusdcFromNoble(path *ibctesting.Path, amount string) channeltypes.Acknowledgement {
	nobleSender := s.chainB.SenderAccount.GetAddress()
	tokenAmt, ok := sdkmath.NewIntFromString(amount)
	s.Require().True(ok)
	coins := sdk.NewCoins(sdk.NewCoin(usdcmigrationtypes.DefaultBaseDenom, tokenAmt))
	nobleBank := s.GetApp(s.chainB).BankKeeper
	s.Require().NoError(nobleBank.MintCoins(s.chainB.GetContext(), ibctransfertypes.ModuleName, coins))
	s.Require().NoError(nobleBank.SendCoinsFromModuleToAccount(s.chainB.GetContext(), ibctransfertypes.ModuleName, nobleSender, coins))

	sendContext := s.chainB.GetContext()
	err := s.TransferFromEndpoint(sendContext, path.EndpointB, ibctransfertypes.FungibleTokenPacketData{
		Denom:    usdcmigrationtypes.DefaultBaseDenom,
		Amount:   amount,
		Sender:   nobleSender.String(),
		Receiver: s.chainA.SenderAccount.GetAddress().String(),
	})
	s.Require().NoError(err)
	packet, err := ParsePacketFromEvents(sendContext.EventManager().ABCIEvents())
	s.Require().NoError(err)
	s.coordinator.CommitBlock(s.chainB)

	s.Require().NoError(path.EndpointA.UpdateClient())
	res, err := path.EndpointA.RecvPacketWithResult(packet)
	s.Require().NoError(err)
	s.coordinator.CommitBlock(s.chainA, s.chainB)

	ackData, err := ParseAckFromEvents(res.Events)
	s.Require().NoError(err)
	var ack channeltypes.Acknowledgement
	s.Require().NoError(ibctransfertypes.ModuleCdc.UnmarshalJSON(ackData, &ack))
	return ack
}

// TestUsdcMigration checks how x/usdcmigration is wired into the Agoric
// transfer stack: it rejects incoming Noble USDC, blocks outgoing transfers of
// the voucher, and sends MsgReturnToNoble packets whose refunds never reach
// the transfer app.
func (s *IntegrationTestSuite) TestUsdcMigration() {
	path := s.NewTransferPath(0, 1)
	agoric := s.GetApp(s.chainA)
	holder := s.chainA.SenderAccount.GetAddress()
	govAddr := authtypes.NewModuleAddress(govtypes.ModuleName)
	voucher := ibctransfertypes.NewDenom(
		usdcmigrationtypes.DefaultBaseDenom,
		ibctransfertypes.NewHop(ibctransfertypes.PortID, path.EndpointA.ChannelID),
	).IBCDenom()

	// Before the module is configured, Noble USDC arrives as usual.
	ack := s.sendUusdcFromNoble(path, "1000")
	s.Require().True(ack.Success(), "%v", ack)
	s.Require().Equal("1000", agoric.BankKeeper.GetBalance(s.chainA.GetContext(), holder, voucher).Amount.String())

	params := usdcmigrationtypes.DefaultParams()
	params.NobleChannels = []string{path.EndpointA.ChannelID}
	s.Require().NoError(agoric.UsdcMigrationKeeper.SetParams(s.chainA.GetContext(), params))
	s.coordinator.CommitBlock(s.chainA)

	s.Run("receive block", func() {
		ack := s.sendUusdcFromNoble(path, "500")
		s.Require().False(ack.Success())
		s.Require().Contains(ack.GetError(), "ABCI code: 3")
		s.Require().Equal("1000", agoric.BankKeeper.GetBalance(s.chainA.GetContext(), holder, voucher).Amount.String())
	})

	s.Run("send block", func() {
		// The transfer keeper burns the voucher before sending, so use a cache
		// context to drop that write as a failed tx would.
		cacheCtx, _ := s.chainA.GetContext().CacheContext()
		err := s.TransferFromEndpoint(cacheCtx, path.EndpointA, ibctransfertypes.FungibleTokenPacketData{
			Denom:    voucher,
			Amount:   "100",
			Sender:   holder.String(),
			Receiver: s.chainB.SenderAccount.GetAddress().String(),
		})
		s.Require().ErrorIs(err, usdcmigrationtypes.ErrSendBlocked)
		s.Require().Equal("1000", agoric.BankKeeper.GetBalance(s.chainA.GetContext(), holder, voucher).Amount.String())
	})

	s.Run("return refund mints nothing", func() {
		// chainB cannot decode a noble1 receiver, so it answers the return with
		// an error ack, which the outermost middleware must swallow.
		receiver, err := bech32.ConvertAndEncode(usdcmigrationtypes.NobleBech32Prefix, s.chainB.SenderAccount.GetAddress())
		s.Require().NoError(err)

		sendContext := s.chainA.GetContext()
		msgServer := usdcmigrationkeeper.NewMsgServerImpl(agoric.UsdcMigrationKeeper)
		_, err = msgServer.ReturnToNoble(sendContext, &usdcmigrationtypes.MsgReturnToNoble{
			Authority: govAddr.String(),
			ChannelId: path.EndpointA.ChannelID,
			Receiver:  receiver,
			Amount:    sdkmath.NewInt(1000),
		})
		s.Require().NoError(err)
		packet, err := ParsePacketFromEvents(sendContext.EventManager().ABCIEvents())
		s.Require().NoError(err)
		s.coordinator.CommitBlock(s.chainA)

		s.Require().NoError(path.EndpointB.UpdateClient())
		res, err := path.EndpointB.RecvPacketWithResult(packet)
		s.Require().NoError(err)
		s.coordinator.CommitBlock(s.chainA, s.chainB)
		ackData, err := ParseAckFromEvents(res.Events)
		s.Require().NoError(err)
		var ack channeltypes.Acknowledgement
		s.Require().NoError(ibctransfertypes.ModuleCdc.UnmarshalJSON(ackData, &ack))
		s.Require().False(ack.Success())

		s.Require().NoError(path.EndpointA.UpdateClient())
		ackRes, err := acknowledgePacketWithResult(path.EndpointA, packet, ackData)
		s.Require().NoError(err)
		s.coordinator.CommitBlock(s.chainA)

		refunded := false
		for _, ev := range ackRes.Events {
			refunded = refunded || ev.Type == usdcmigrationtypes.EventTypeReturnRefunded
		}
		s.Require().True(refunded, "expected %s event", usdcmigrationtypes.EventTypeReturnRefunded)

		ctx := s.chainA.GetContext()
		s.Require().True(agoric.BankKeeper.GetBalance(ctx, govAddr, voucher).IsZero())
		s.Require().Equal("1000", agoric.BankKeeper.GetSupply(ctx, voucher).Amount.String())
	})
}
