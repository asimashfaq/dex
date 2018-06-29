package dex

import (
	"fmt"
	"math"
	"math/big"
	"strings"

	"github.com/ethereum/go-ethereum/rlp"
	"github.com/helinwang/dex/pkg/consensus"
	log "github.com/helinwang/log15"
)

type Transition struct {
	round           uint64
	finalized       bool
	tokenCreations  []Token
	txns            []*consensus.Txn
	expirations     map[uint64][]orderExpiration
	filledOrders    []PendingOrder
	state           *State
	orderBooks      map[MarketSymbol]*orderBook
	dirtyOrderBooks map[MarketSymbol]bool
	tokenCache      *TokenCache
}

func newTransition(s *State, round uint64) *Transition {
	return &Transition{
		state:           s,
		round:           round,
		expirations:     make(map[uint64][]orderExpiration),
		orderBooks:      make(map[MarketSymbol]*orderBook),
		dirtyOrderBooks: make(map[MarketSymbol]bool),
		tokenCache:      newTokenCache(s),
		filledOrders:    make([]PendingOrder, 0, 1000), // optimization: preallocate buffer
	}
}

func (t *Transition) RecordSerialized(blob []byte, pool consensus.TxnPool) (count int, valid, success bool) {
	var txns [][]byte
	err := rlp.DecodeBytes(blob, &txns)
	if err != nil {
		log.Error("error decode txns in RecordTxns", "err", err)
		return
	}

	for _, b := range txns {
		hash := consensus.SHA3(b)
		txn := pool.Get(hash)
		if txn == nil {
			txn, _ = pool.Add(b)
		}
		valid, success = t.Record(txn)
		if !valid || !success {
			log.Error("error record txn in encoded txns", "valid", valid, "success", success, "hash", hash)
			return
		}
		pool.Remove(hash)
	}

	return len(txns), true, true
}

// Record records a transition to the state transition.
func (t *Transition) Record(txn *consensus.Txn) (valid, success bool) {
	if t.finalized {
		panic("record should never be called after finalized")
	}

	acc, ready, nonceValid := validateNonce(t.state, txn)
	if !nonceValid {
		return
	}

	if !ready {
		return true, false
	}

	// TODO: encode txn.data more efficiently to save network bandwidth
	switch tx := txn.Decoded.(type) {
	case *PlaceOrderTxn:
		if !t.placeOrder(acc, tx, t.round) {
			log.Warn("placeOrder failed")
			return
		}
	case *CancelOrderTxn:
		if !t.cancelOrder(acc, tx) {
			log.Warn("cancelOrder failed")
			return
		}
	case *IssueTokenTxn:
		if !t.issueToken(acc, tx) {
			log.Warn("CreateTokenTxn failed")
			return
		}
	case *SendTokenTxn:
		if !t.sendToken(acc, tx) {
			log.Warn("SendTokenTxn failed")
			return
		}
	case *FreezeTokenTxn:
		if !t.freezeToken(acc, tx) {
			log.Warn("FreezeTokenTxn failed")
			return
		}

	default:
		log.Warn("unknown txn type", "type", fmt.Sprintf("%T", txn.Decoded))
		return false, false
	}

	t.txns = append(t.txns, txn)
	return true, true
}

func (t *Transition) getOrderBook(m MarketSymbol) *orderBook {
	book := t.orderBooks[m]
	if book == nil {
		book = t.state.loadOrderBook(m)
		if book == nil {
			book = newOrderBook()
		}
		t.orderBooks[m] = book
	}

	return book
}

func calcQuoteQuant(baseQuantUnit uint64, quoteDecimals uint8, priceQuantUnit uint64, priceDecimals, baseDecimals uint8) uint64 {
	var quantUnit big.Int
	var quoteDenominator big.Int
	var priceU big.Int
	var priceDenominator big.Int
	var baseDenominator big.Int
	quantUnit.SetUint64(baseQuantUnit)
	quoteDenominator.SetUint64(uint64(math.Pow10(int(quoteDecimals))))
	priceU.SetUint64(priceQuantUnit)
	priceDenominator.SetUint64(uint64(math.Pow10(int(OrderPriceDecimals))))
	baseDenominator.SetUint64(uint64(math.Pow10(int(baseDecimals))))
	var result big.Int
	result.Mul(&quantUnit, &quoteDenominator)
	result.Mul(&result, &priceU)
	result.Div(&result, &baseDenominator)
	result.Div(&result, &priceDenominator)
	return result.Uint64()
}

func (t *Transition) cancelOrder(owner *Account, txn *CancelOrderTxn) bool {
	cancel, ok := owner.PendingOrder(txn.ID)
	if !ok {
		log.Warn("can not find the order to cancel", "id", txn.ID)
		return false
	}

	book := t.getOrderBook(txn.ID.Market)
	book.Cancel(txn.ID.ID)
	t.dirtyOrderBooks[txn.ID.Market] = true
	owner.RemovePendingOrder(txn.ID)
	t.refundAfterCancel(owner, cancel, txn.ID.Market)
	return true
}

func (t *Transition) refundAfterCancel(owner *Account, cancel PendingOrder, market MarketSymbol) {
	if cancel.Quant <= cancel.Executed {
		panic(fmt.Errorf("pending order remain amount should be greater than 0, total: %d, executed: %d", cancel.Quant, cancel.Executed))
	}

	refund := cancel.Quant - cancel.Executed
	if cancel.SellSide {
		baseBalance := owner.Balance(market.Base)

		if baseBalance.Pending < refund {
			panic(fmt.Errorf("pending balance smaller than refund, pending: %d, refund: %d", baseBalance.Pending, refund))
		}

		baseBalance.Pending -= refund
		baseBalance.Available += refund
		owner.UpdateBalance(market.Base, baseBalance)
	} else {
		quoteBalance := owner.Balance(market.Quote)
		fmt.Println(quoteBalance, market.Quote, refund)
		quoteInfo := t.tokenCache.idToInfo[market.Quote]
		baseInfo := t.tokenCache.idToInfo[market.Base]
		pendingQuant := calcQuoteQuant(refund, quoteInfo.Decimals, cancel.Price, OrderPriceDecimals, baseInfo.Decimals)

		if quoteBalance.Pending < pendingQuant {
			panic(fmt.Errorf("pending balance smaller than refund, pending: %d, refund: %d", quoteBalance.Pending, pendingQuant))
		}

		quoteBalance.Pending -= pendingQuant
		quoteBalance.Available += pendingQuant
		owner.UpdateBalance(market.Quote, quoteBalance)
	}
}

type ExecutionReport struct {
	Round      uint64
	ID         OrderID
	SellSide   bool
	TradePrice uint64
	Quant      uint64
	Fee        uint64
}

func (t *Transition) placeOrder(owner *Account, txn *PlaceOrderTxn, round uint64) bool {
	if !txn.Market.Valid() {
		log.Warn("order's market is invalid", "market", txn.Market)
		return false
	}
	if txn.ExpireRound > 0 && round >= txn.ExpireRound {
		log.Warn("order already expired", "expire round", txn.ExpireRound, "cur round", round)
		return false
	}

	baseInfo := t.tokenCache.Info(txn.Market.Base)
	if baseInfo == nil {
		log.Warn("trying to place order on nonexistent token", "token", txn.Market.Base)
		return false
	}

	quoteInfo := t.tokenCache.Info(txn.Market.Quote)
	if quoteInfo == nil {
		log.Warn("trying to place order on nonexistent token", "token", txn.Market.Quote)
		return false
	}

	if txn.SellSide {
		if txn.Quant == 0 {
			log.Warn("sell: can not sell 0 quantity")
			return false
		}

		baseBalance := owner.Balance(txn.Market.Base)
		if baseBalance.Available < txn.Quant {
			log.Warn("sell failed: insufficient balance", "quant", txn.Quant, "available", baseBalance.Available)
			return false
		}

		baseBalance.Available -= txn.Quant
		baseBalance.Pending += txn.Quant
		owner.UpdateBalance(txn.Market.Base, baseBalance)
	} else {
		if txn.Quant == 0 {
			log.Warn("buy failed: can not buy 0 quantity")
			return false
		}

		pendingQuant := calcQuoteQuant(txn.Quant, quoteInfo.Decimals, txn.Price, OrderPriceDecimals, baseInfo.Decimals)
		if pendingQuant == 0 {
			log.Warn("buy failed: converted quote quant is 0")
			return false
		}

		quoteBalance := owner.Balance(txn.Market.Quote)
		if quoteBalance.Available < pendingQuant {
			log.Warn("buy failed, insufficient balance", "required", pendingQuant, "available", quoteBalance.Available)
			return false
		}

		quoteBalance.Available -= pendingQuant
		quoteBalance.Pending += pendingQuant
		owner.UpdateBalance(txn.Market.Quote, quoteBalance)
	}

	order := Order{
		Owner:       owner.PK().Addr(),
		SellSide:    txn.SellSide,
		Quant:       txn.Quant,
		Price:       txn.Price,
		ExpireRound: txn.ExpireRound,
	}

	book := t.getOrderBook(txn.Market)
	orderID, executions := book.Limit(order)
	t.dirtyOrderBooks[txn.Market] = true
	id := OrderID{ID: orderID, Market: txn.Market}
	pendingOrder := PendingOrder{
		ID:    id,
		Order: order,
	}
	owner.UpdatePendingOrder(pendingOrder)
	if order.ExpireRound > 0 {
		t.expirations[order.ExpireRound] = append(t.expirations[order.ExpireRound], orderExpiration{ID: id, Owner: owner.PK().Addr()})
	}

	if len(executions) > 0 {
		for _, exec := range executions {
			acc := t.state.Account(exec.Owner)
			// TODO: report fee
			orderID := OrderID{ID: exec.ID, Market: txn.Market}
			report := ExecutionReport{
				Round:      round,
				ID:         orderID,
				SellSide:   exec.SellSide,
				TradePrice: exec.Price,
				Quant:      exec.Quant,
			}
			acc.AddExecutionReport(report)
			executedOrder, ok := acc.PendingOrder(orderID)
			if !ok {
				panic(fmt.Errorf("impossible: can not find matched order %d, market: %v, executed order: %v", exec.ID, txn.Market, exec))
			}

			executedOrder.Executed += exec.Quant
			if executedOrder.Executed == executedOrder.Quant {
				acc.RemovePendingOrder(orderID)
				t.filledOrders = append(t.filledOrders, executedOrder)
			} else {
				acc.UpdatePendingOrder(executedOrder)
			}

			baseBalance := acc.Balance(txn.Market.Base)
			quoteBalance := acc.Balance(txn.Market.Quote)
			if exec.SellSide {
				if baseBalance.Pending < exec.Quant {
					panic(fmt.Errorf("insufficient pending balance, owner: %v, pending %d, executed: %d, sell side, taker: %t", exec.Owner, baseBalance.Pending, exec.Quant, exec.Taker))
				}

				baseBalance.Pending -= exec.Quant
				recvQuant := calcQuoteQuant(exec.Quant, quoteInfo.Decimals, exec.Price, OrderPriceDecimals, baseInfo.Decimals)
				quoteBalance.Available += recvQuant
				acc.UpdateBalance(txn.Market.Base, baseBalance)
				acc.UpdateBalance(txn.Market.Quote, quoteBalance)
			} else {
				recvQuant := exec.Quant
				pendingQuant := calcQuoteQuant(exec.Quant, quoteInfo.Decimals, executedOrder.Price, OrderPriceDecimals, baseInfo.Decimals)
				givenQuant := calcQuoteQuant(exec.Quant, quoteInfo.Decimals, exec.Price, OrderPriceDecimals, baseInfo.Decimals)

				if quoteBalance.Pending < pendingQuant {
					panic(fmt.Errorf("insufficient pending balance, owner: %v, pending %d, executed: %d, buy side, taker: %t", exec.Owner, quoteBalance.Pending, exec.Quant, exec.Taker))
				}

				quoteBalance.Pending -= pendingQuant
				quoteBalance.Available += pendingQuant
				quoteBalance.Available -= givenQuant
				baseBalance.Available += recvQuant
				acc.UpdateBalance(txn.Market.Base, baseBalance)
				acc.UpdateBalance(txn.Market.Quote, quoteBalance)
			}
		}
	}
	return true
}

func (t *Transition) issueToken(owner *Account, txn *IssueTokenTxn) bool {
	if t.tokenCache.Exists(txn.Info.Symbol) {
		log.Warn("token symbol already exists", "symbol", txn.Info.Symbol)
		return false
	}

	for _, v := range t.tokenCreations {
		if strings.ToUpper(string(txn.Info.Symbol)) == strings.ToUpper(string(v.Symbol)) {
			log.Warn("token symbol already exists in the current transition", "symbol", txn.Info.Symbol)
			return false
		}
	}

	// TODO: fiture out how to pay fee.
	id := TokenID(t.tokenCache.Size() + len(t.tokenCreations))
	token := Token{ID: id, TokenInfo: txn.Info}
	t.tokenCreations = append(t.tokenCreations, token)
	t.state.UpdateToken(token)
	owner.UpdateBalance(id, Balance{Available: txn.Info.TotalUnits})
	return true
}

// TODO: all elements in trie should be serialized using rlp, not gob,
// since gob is not deterministic.
func (t *Transition) sendToken(owner *Account, txn *SendTokenTxn) bool {
	if txn.Quant == 0 {
		return false
	}

	b := owner.Balance(txn.TokenID)
	if b.Available < txn.Quant {
		log.Warn("insufficient available token balance", "tokenID", txn.TokenID, "quant", txn.Quant, "available", b.Available)
		return false
	}

	toAddr := txn.To.Addr()
	toAcc := t.state.Account(toAddr)
	if toAcc == nil {
		toAcc = t.state.NewAccount(txn.To)
	}

	b.Available -= txn.Quant
	owner.UpdateBalance(txn.TokenID, b)
	toAccBalance := toAcc.Balance(txn.TokenID)
	toAccBalance.Available += txn.Quant
	toAcc.UpdateBalance(txn.TokenID, toAccBalance)
	return true
}

func (t *Transition) Txns() []byte {
	if len(t.txns) == 0 {
		return nil
	}

	bs := make([][]byte, len(t.txns))
	for i := range bs {
		bs[i] = t.txns[i].Raw
	}

	b, err := rlp.EncodeToBytes(bs)
	if err != nil {
		panic(err)
	}

	return b
}

func (t *Transition) finalizeState(round uint64) {
	if !t.finalized {
		t.removeFilledOrderFromExpiration()
		// must be called after
		// t.removeFilledOrderFromExpiration
		t.recordOrderExpirations()
		// must be called after t.recordOrderExpirations,
		// since current round may add expiring orders for the
		// next round.
		t.expireOrders()
		// must be called after t.expireOrders, since it could
		// make order book dirty.
		t.saveDirtyOrderBooks()
		t.releaseTokens()
		t.state.CommitCache()
		t.finalized = true
	}
}

func (t *Transition) recordOrderExpirations() {
	for expireRound, ids := range t.expirations {
		t.state.AddOrderExpirations(expireRound, ids)
	}
}

func (t *Transition) saveDirtyOrderBooks() {
	for m, b := range t.orderBooks {
		if t.dirtyOrderBooks[m] {
			t.state.saveOrderBook(m, b)
		}
	}
}

func (t *Transition) removeFilledOrderFromExpiration() {
	rounds := make(map[uint64]int)
	filled := make(map[OrderID]bool)
	for _, o := range t.filledOrders {
		if o.ExpireRound == 0 {
			continue
		}

		filled[o.ID] = true
		rounds[o.ExpireRound]++
	}

	for round, toRemove := range rounds {
		// remove filled order's expiration from the
		// to-be-added expirations of this round.
		expirations := t.expirations[round]
		newExpirations := make([]orderExpiration, 0, len(expirations))
		for _, exp := range expirations {
			if !filled[exp.ID] {
				newExpirations = append(newExpirations, exp)
			}
		}
		t.expirations[round] = newExpirations
		removed := len(newExpirations) - len(expirations)
		if removed == toRemove {
			continue
		}

		// remove filled order's expiration from the saved
		// expiration from disk.
		t.state.RemoveOrderExpirations(round, filled)
	}
}

func (t *Transition) releaseTokens() {
	// release the tokens that will be released next round
	tokens := t.state.GetFreezeTokens(t.round + 1)
	addrToAcc := make(map[consensus.Addr]*Account)
	for _, token := range tokens {
		acc, ok := addrToAcc[token.Addr]
		if !ok {
			acc = t.state.Account(token.Addr)
			addrToAcc[token.Addr] = acc
		}

		b := acc.Balance(token.TokenID)
		removeIdx := -1
		for i, f := range b.Frozen {
			if f.Quant == token.Quant {
				removeIdx = i
				break
			}
		}
		f := b.Frozen[removeIdx]
		b.Frozen = append(b.Frozen[:removeIdx], b.Frozen[removeIdx+1:]...)
		b.Available += f.Quant
		acc.UpdateBalance(token.TokenID, b)
	}
}

func (t *Transition) expireOrders() {
	// expire orders whose expiration is the next round
	orders := t.state.GetOrderExpirations(t.round + 1)
	addrToAcc := make(map[consensus.Addr]*Account)
	for _, o := range orders {
		t.getOrderBook(o.ID.Market).Cancel(o.ID.ID)
		t.dirtyOrderBooks[o.ID.Market] = true

		acc, ok := addrToAcc[o.Owner]
		if !ok {
			acc = t.state.Account(o.Owner)
			addrToAcc[o.Owner] = acc
		}

		order, ok := acc.PendingOrder(o.ID)
		if !ok {
			panic("can not find expiring order")
		}

		acc.RemovePendingOrder(o.ID)
		t.refundAfterCancel(acc, order, o.ID.Market)
	}
}

func (t *Transition) freezeToken(acc *Account, txn *FreezeTokenTxn) bool {
	if txn.Quant == 0 {
		return false
	}

	if txn.AvailableRound <= t.round {
		log.Warn("trying to freeze token to too early round", "available round", txn.AvailableRound, "cur round", t.round)
		return false
	}

	b := acc.Balance(txn.TokenID)

	if b.Available < txn.Quant {
		log.Warn("insufficient available token balance", "tokenID", txn.TokenID, "quant", txn.Quant, "available", b.Available)
		return false
	}

	frozen := Frozen{
		AvailableRound: txn.AvailableRound,
		Quant:          txn.Quant,
	}
	b.Available -= txn.Quant
	b.Frozen = append(b.Frozen, frozen)
	acc.UpdateBalance(txn.TokenID, b)
	t.state.FreezeToken(txn.AvailableRound, freezeToken{Addr: acc.PK().Addr(), TokenID: txn.TokenID, Quant: txn.Quant})
	return true
}

func (t *Transition) StateHash() consensus.Hash {
	t.finalizeState(t.round)
	return t.state.Hash()
}

func (t *Transition) Commit() consensus.State {
	t.finalizeState(t.round)
	for _, v := range t.tokenCreations {
		t.tokenCache.Update(v.ID, &v.TokenInfo)
	}

	return t.state
}
