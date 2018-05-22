package target

import (
	"fmt"

	"github.com/SmartMeshFoundation/SmartRaiden/log"
	"github.com/SmartMeshFoundation/SmartRaiden/transfer"
	"github.com/SmartMeshFoundation/SmartRaiden/transfer/mediated_transfer"
	"github.com/SmartMeshFoundation/SmartRaiden/transfer/mediated_transfer/mediator"
	"github.com/SmartMeshFoundation/SmartRaiden/utils"
)

//NameTargetTransition name for state manager
const NameTargetTransition = "TargetTransition"

func init() {
}

/*
Emits the event for closing the netting channel if from_transfer needs
    to be settled on-chain.
*/
func eventsForClose(state *mediated_transfer.TargetState) (events []transfer.Event) {
	fromTransfer := state.FromTransfer
	fromRoute := state.FromRoute
	safeToWait := mediator.IsSafeToWait(fromTransfer, fromRoute.RevealTimeout, state.BlockNumber)
	secretKnown := fromTransfer.Secret != utils.EmptyHash
	if !safeToWait && secretKnown {
		state.State = mediated_transfer.StateWaitingClose
		channelClose := &mediated_transfer.EventContractSendChannelClose{
			ChannelAddress: fromRoute.ChannelAddress,
			Token:          fromTransfer.Token,
		}
		events = append(events, channelClose)
	}
	return
}

//Withdraw from the from_channel if it is closed and the secret is known.
func eventsForWithdraw(state *mediated_transfer.TargetState, fromRoute *transfer.RouteState) (events []transfer.Event) {
	fromTransfer := state.FromTransfer
	if state.Db != nil {
		ch, err := state.Db.GetChannelByAddress(fromRoute.ChannelAddress)
		if err != nil {
			log.Error(fmt.Sprintf("get channel %s from db err %s", utils.APex(fromRoute.ChannelAddress), err))
		} else {
			fromRoute.State = ch.State
		}
	} else {
		log.Error(" db is nil can only be ignored when you are run testing...")
	}
	isChannelOpen := fromRoute.State == transfer.ChannelStateOpened
	if !isChannelOpen && fromTransfer.Secret != utils.EmptyHash { //重复发送，直到取现成功？或者expired？
		if state.Db != nil {
			if state.Db.IsThisLockHasWithdraw(fromRoute.ChannelAddress, fromTransfer.Secret) {
				return
			}
		}
		withdraw := &mediated_transfer.EventContractSendWithdraw{
			Transfer:       fromTransfer,
			ChannelAddress: fromRoute.ChannelAddress,
		}
		events = append(events, withdraw)
	}
	return
}

//Handle an ActionInitTarget state change.
func handleInitTraget(st *mediated_transfer.ActionInitTargetStateChange) *transfer.TransitionResult {
	tr := st.FromTranfer
	route := st.FromRoute
	blockNumber := st.BlockNumber
	state := &mediated_transfer.TargetState{
		OurAddress:   st.OurAddress,
		FromRoute:    route,
		FromTransfer: tr,
		BlockNumber:  blockNumber,
		Db:           st.Db,
	}
	safeToWait := mediator.IsSafeToWait(tr, route.RevealTimeout, blockNumber)
	/*
			  if there is not enough time to safely withdraw the token on-chain
		     silently let the transfer expire.
	*/
	if safeToWait {
		secretRequest := &mediated_transfer.EventSendSecretRequest{
			Identifer: tr.Identifier,
			Amount:    tr.Amount,
			Hashlock:  tr.Hashlock,
			Receiver:  tr.Initiator,
		}
		return &transfer.TransitionResult{
			NewState: state,
			Events:   []transfer.Event{secretRequest},
		}
	}
	//如果超时了,那就什么都不做,等待相关各方自己取消?
	return &transfer.TransitionResult{
		NewState: state,
		Events:   nil,
	}
}

// Validate and handle a ReceiveSecretReveal state change.
func handleSecretReveal(state *mediated_transfer.TargetState, st *mediated_transfer.ReceiveSecretRevealStateChange) (it *transfer.TransitionResult) {
	validSecret := utils.Sha3(st.Secret[:]) == state.FromTransfer.Hashlock
	var events []transfer.Event
	if validSecret {
		tr := state.FromTransfer
		route := state.FromRoute
		state.State = mediated_transfer.StateRevealSecret
		tr.Secret = st.Secret
		reveal := &mediated_transfer.EventSendRevealSecret{
			Identifier: tr.Identifier,
			Secret:     tr.Secret,
			Token:      tr.Token,
			Receiver:   route.HopNode,
			Sender:     state.OurAddress,
		}
		events = append(events, reveal)
	} else {
		// TODO: event for byzantine behavior
	}
	it = &transfer.TransitionResult{
		NewState: state,
		Events:   events,
	}
	return
}

func handleBalanceProof(state *mediated_transfer.TargetState, st *mediated_transfer.ReceiveBalanceProofStateChange) (it *transfer.TransitionResult) {
	it = &transfer.TransitionResult{
		NewState: state,
		Events:   nil,
	}
	//TODO: byzantine behavior event when the sender doesn't match
	if st.NodeAddress == state.FromRoute.HopNode {
		state.State = mediated_transfer.StateBalanceProof
	}
	return
}

/*
After Raiden learns about a new block this function must be called to
    handle expiration of the hash time lock.
*/
func handleBlock(state *mediated_transfer.TargetState, st *transfer.BlockStateChange) (it *transfer.TransitionResult) {
	if state.BlockNumber < st.BlockNumber {
		state.BlockNumber = st.BlockNumber
	}
	/*
	   only emit the close event once

	*/
	var events []transfer.Event
	if state.State != mediated_transfer.StateWaitingClose {
		events = eventsForClose(state)
	}
	events2 := eventsForWithdraw(state, state.FromRoute)
	events = append(events, events2...)
	it = &transfer.TransitionResult{
		NewState: state,
		Events:   events,
	}
	return
}

func handleRouteChange(state *mediated_transfer.TargetState, st *transfer.ActionRouteChangeStateChange) (it *transfer.TransitionResult) {
	if st.Route.HopNode != state.FromRoute.HopNode {
		panic("updated_route.node_address == state.from_route.node_address")
	}
	/*
		the route might be closed by another task
	*/
	state.FromRoute = st.Route
	withdrawEvents := eventsForWithdraw(state, state.FromRoute)
	it = &transfer.TransitionResult{
		NewState: state,
		Events:   withdrawEvents,
	}
	return
}

//Clear the state if the transfer was either completed or failed
func clearIfFinalized(previt *transfer.TransitionResult) (it *transfer.TransitionResult) {
	if previt.NewState == nil {
		return previt
	}
	state, ok := previt.NewState.(*mediated_transfer.TargetState)
	if !ok {
		panic(fmt.Sprintf("clearIfFinalized for targetstate type error:%s", utils.StringInterface1(previt)))
	}
	it = previt
	if state.FromTransfer.Secret == utils.EmptyHash && state.BlockNumber > state.FromTransfer.Expiration {
		failed := &mediated_transfer.EventWithdrawFailed{
			Identifier:     state.FromTransfer.Identifier,
			Hashlock:       state.FromTransfer.Hashlock,
			ChannelAddress: state.FromRoute.ChannelAddress,
			Reason:         "lock expired",
		}
		it = &transfer.TransitionResult{
			NewState: nil,
			Events:   []transfer.Event{failed},
		}
	} else if state.State == mediated_transfer.StateBalanceProof {
		//这些事件对应的处理都没有
		transferSuccess := &transfer.EventTransferReceivedSuccess{
			Identifier: state.FromTransfer.Identifier,
			Amount:     state.FromTransfer.Amount,
			Initiator:  state.FromTransfer.Initiator,
		}
		unlockSuccess := &mediated_transfer.EventWithdrawSuccess{
			Identifier: state.FromTransfer.Identifier,
			Hashlock:   state.FromTransfer.Hashlock,
		}
		it = &transfer.TransitionResult{
			NewState: nil,
			Events:   []transfer.Event{transferSuccess, unlockSuccess},
		}
	}
	return it
}

// StateTransiton is State machine for the target node of a target transfer.
func StateTransiton(originalState transfer.State, stateChange transfer.StateChange) (it *transfer.TransitionResult) {
	it = &transfer.TransitionResult{
		NewState: originalState,
		Events:   nil,
	}
	if originalState == nil {
		ait, ok := stateChange.(*mediated_transfer.ActionInitTargetStateChange)
		if ok {
			it = handleInitTraget(ait)
		}
	} else {
		state, ok := originalState.(*mediated_transfer.TargetState)
		if !ok {
			panic(fmt.Sprintf("targetstate StateTransiton type error:%s", utils.StringInterface1(originalState)))
		}
		if state.FromTransfer.Secret == utils.EmptyHash {
			switch st2 := stateChange.(type) {
			case *mediated_transfer.ReceiveSecretRevealStateChange:
				it = handleSecretReveal(state, st2)
			case *transfer.BlockStateChange:
				it = handleBlock(state, st2)
			}
		} else if state.FromTransfer.Secret != utils.EmptyHash {
			switch st2 := stateChange.(type) {
			case *mediated_transfer.ReceiveBalanceProofStateChange:
				it = handleBalanceProof(state, st2)
				//目前没用
			case *transfer.ActionRouteChangeStateChange:
				it = handleRouteChange(state, st2)
			case *transfer.BlockStateChange:
				it = handleBlock(state, st2)
			}
		}
	}
	return clearIfFinalized(it)
}
