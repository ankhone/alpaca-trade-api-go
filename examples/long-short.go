package main

import (
	"fmt"
	"math"
	"os"
	"sort"
	"strconv"
	"time"

	"github.com/alpacahq/alpaca-trade-api-go/alpaca"
	"github.com/alpacahq/alpaca-trade-api-go/common"
	"github.com/shopspring/decimal"
)

type alpacaClientContainer struct {
	client    *alpaca.Client
	long      bucket
	short     bucket
	allStocks []stockField
	blacklist []string
}
type bucket struct {
	bucketType  string
	list        []string
	qty         int
	adjustedQty int
	equityAmt   float64
}
type stockField struct {
	name string
	pc   float64
}

var alpacaClient alpacaClientContainer

func init() {
	os.Setenv(common.EnvApiKeyID, "PKX3X3ZVWFHT0OM4WTFF")
	os.Setenv(common.EnvApiSecretKey, "v5UqSbrF/dbcOHeH2B7GAhSQFX9m/AFxpiWrk1Qy")
	alpaca.SetBaseUrl("https://paper-api.alpaca.markets")

	// Format the allStocks variable for use in the class
	allStocks := make([]stockField, 0)
	stockList := []string{"DOMO", "TLRY", "SQ", "MRO", "AAPL", "GM", "SNAP", "SHOP", "SPLK", "BA", "AMZN", "SUI", "SUN", "TSLA", "CGC", "SPWR", "NIO", "CAT", "MSFT", "PANW", "OKTA", "TWTR", "TM", "RTN", "ATVI", "GS", "BAC", "MS", "TWLO", "QCOM"}
	for _, stock := range stockList {
		allStocks = append(allStocks, stockField{stock, 0})
	}

	alpacaClient = alpacaClientContainer{
		alpaca.NewClient(common.Credentials()),
		bucket{"Long", make([]string, 0), -1, -1, 0},
		bucket{"Short", make([]string, 0), -1, -1, 0},
		make([]stockField, len(allStocks)),
		make([]string, 0, len(allStocks)),
	}

	copy(alpacaClient.allStocks, allStocks)
}

func main() {
	// First, cancel any existing orders so they don't impact our buying power.
	status, until, limit := "open", time.Now(), 100
	orders, _ := alpacaClient.client.ListOrders(&status, &until, &limit)
	for _, order := range orders {
		_ = alpacaClient.client.CancelOrder(order.ID)
	}

	// Wait for market to open
	cAMO := make(chan bool)
	fmt.Println("Waiting for market to open...")
	for {
		go alpacaClient.awaitMarketOpen(cAMO)
		if <-cAMO {
			break
		}
		time.Sleep(2000 * time.Millisecond)
	}
	fmt.Println("Market Opened.")

	for {
		cRun := make(chan bool)
		go alpacaClient.run(cRun)
		<-cRun
		fmt.Println("End")
	}
}

// Rebalance the portfolio every minute, making necessary trades
func (alp alpacaClientContainer) run(cRun chan bool) {
	cRebalance := make(chan bool)
	go alp.rebalance(cRebalance)
	<-cRebalance

	// Figure out when the market will close so we can prepare to sell beforehand.
	clock, _ := alp.client.GetClock()
	timeToClose := int((clock.NextClose.UnixNano() - clock.Timestamp.UnixNano()) / 1000000)
	if timeToClose < 60000*15 {
		// Close all positions when 15 minutes til market close
		fmt.Println("Market closing soon.  Closing positions.")

		positions, _ := alp.client.ListPositions()
		for _, position := range positions {
			var orderSide string
			if position.Side == "long" {
				orderSide = "sell"
			} else {
				orderSide = "buy"
			}
			qty, _ := position.Qty.Float64()
			qty = math.Abs(qty)
			cSubmitOrder := make(chan error)
			go alp.submitOrder(int(qty), position.Symbol, orderSide, cSubmitOrder)
			<-cSubmitOrder
			// Run script again after market close for next trading day
			time.Sleep((60000 * 15) * time.Millisecond)
		}
	} else {
		time.Sleep(60000 * time.Millisecond)
	}
	cRun <- true
}

// Spin until the market is open
func (alp alpacaClientContainer) awaitMarketOpen(cAMO chan bool) {
	clock, _ := alp.client.GetClock()
	if clock.IsOpen {
		cAMO <- true
	} else {
		fmt.Println("spinning")
		cAMO <- true
	}
	cAMO <- true
	return
}

// Rebalance our position after an update
func (alp alpacaClientContainer) rebalance(cRebalance chan bool) {
	cRank := make(chan bool)
	go alp.rerank(cRank)
	<-cRank
	fmt.Println("Out of rerank")

	// Clear existing orders again
	status, until, limit := "open", time.Now(), 100
	orders, _ := alp.client.ListOrders(&status, &until, &limit)
	for _, order := range orders {
		_ = alp.client.CancelOrder(order.ID)
	}

	// Remove positions that are no longer in the short or long list, and make a list of positions that do not need to change.  Adjust position quantities if needed.
	var executed [2][]string
	positions, _ := alp.client.ListPositions()
	for _, position := range positions {
		cIndexOfLong := make(chan int)
		go indexOf(alpacaClient.long.list, position.Symbol, cIndexOfLong)
		indLong := <-cIndexOfLong

		cIndexOfShort := make(chan int)
		go indexOf(alpacaClient.short.list, position.Symbol, cIndexOfShort)
		indShort := <-cIndexOfShort

		rawQty, _ := position.Qty.Float64()
		qty := int(math.Abs(rawQty))
		side := "buy"
		if indLong < 0 {
			// Position is not in long list
			if indShort < 0 {
				// Position not in short list either.  Clear position
				if position.Side == "long" {
					qty = qty + alp.short.qty
					side = "sell"
				} else {
					side = "buy"
				}
			} else {
				// Position in short list
				if qty == alp.short.qty {
					// Position is where we want it.  Pass for now
					qty = 0
				} else {
					// Need to adjust position amount
					diff := qty - alp.short.qty
					if diff > 0 {
						// Too many short positions.  Buy some back to rebalance.
						side = "buy"
					} else {
						// Too little short positions.  Sell some more.
						diff = int(math.Abs(float64(diff)))
						side = "sell"
					}
					qty = diff
				}
			}
		} else {
			// Position in long list
			if position.Side == "short" {
				// Position changed from short to long.  Clear short position to prep for long purchase.
				qty = qty + alp.long.qty
				side = "buy"
			} else {
				if qty == alp.long.qty {
					// Position is where we want it.  Pass for now
					qty = 0
				} else {
					// Need to adjust position amount
					diff := qty - alp.long.qty
					if diff > 0 {
						// Too many long positions.  Sell some to rebalance
						side = "sell"
					} else {
						diff = int(math.Abs(float64(diff)))
						side = "buy"
					}
					qty = diff
				}
			}
		}
		cSubmitOrder := make(chan error)
		go alpacaClient.submitOrder(qty, position.Symbol, side, cSubmitOrder)
		<-cSubmitOrder
		// Blacklist position so that duplicate orders/adjustments are not made
		alpacaClient.blacklist = append(alpacaClient.blacklist, position.Symbol)
		if side == "buy" {
			executed[0] = append(executed[0], position.Symbol)
		} else {
			executed[1] = append(executed[1], position.Symbol)
		}
	}
	fmt.Println("Out of adjustments")

	// Send orders to all remaining stocks in the long and short list
	cSendBOLong := make(chan [2][]string)
	go alp.sendBatchOrder(alpacaClient.long.qty, alpacaClient.long.list, "buy", cSendBOLong)
	longBOResp := <-cSendBOLong
	executed[0] = append(executed[0], longBOResp[0][:]...)
	if len(longBOResp[1][:]) > 0 {
		// Handle rejected/incomplete orders and determine new quantities to purchase
		cGetTPLong := make(chan float64)
		go alp.getTotalPrice(longBOResp[0][:], cGetTPLong)
		longTPResp := <-cGetTPLong
		if longTPResp > 0 {
			alpacaClient.long.adjustedQty = int(alpacaClient.long.equityAmt / <-cGetTPLong)
		} else {
			alpacaClient.long.adjustedQty = -1
		}
	} else {
		alpacaClient.long.adjustedQty = -1
	}

	cSendBOShort := make(chan [2][]string)
	go alp.sendBatchOrder(alpacaClient.short.qty, alpacaClient.short.list, "sell", cSendBOShort)
	shortBOResp := <-cSendBOShort
	executed[1] = append(executed[1], shortBOResp[0][:]...)
	if len(shortBOResp[1][:]) > 0 {
		// Handle rejected/incomplete orders and determine new quantities to purchase
		cGetTPShort := make(chan float64)
		go alp.getTotalPrice(shortBOResp[0][:], cGetTPShort)
		shortTPResp := <-cGetTPShort
		if shortTPResp > 0 {
			alpacaClient.short.adjustedQty = int(alpacaClient.short.equityAmt / <-cGetTPShort)
		} else {
			alpacaClient.short.adjustedQty = -1
		}
	} else {
		alpacaClient.short.adjustedQty = -1
	}

	fmt.Println("Out of first orders")

	// Reorder stocks that didn't throw an error so that the equity quota is reached
	if alpacaClient.long.adjustedQty > -1 {
		alpacaClient.long.qty = alpacaClient.long.adjustedQty - alpacaClient.long.qty
		cResendBOLong := make(chan [2][]string)
		go alp.sendBatchOrder(alpacaClient.long.qty, alpacaClient.long.list, "buy", cResendBOLong)
		<-cResendBOLong
	}

	if alpacaClient.short.adjustedQty > -1 {
		alpacaClient.short.qty = alpacaClient.short.adjustedQty - alpacaClient.short.qty
		cResendBOShort := make(chan [2][]string)
		go alp.sendBatchOrder(alpacaClient.short.qty, alpacaClient.short.list, "sell", cResendBOShort)
		<-cResendBOShort
	}
	fmt.Println("Out of reorders")
	cRebalance <- true
}

// Re-rank all stocks to adjust longs and shorts
func (alp alpacaClientContainer) rerank(cRerank chan bool) {
	cRank := make(chan bool)
	go alp.rank(cRank)
	<-cRank
	fmt.Println("Out of rank")

	// Grabs the top and bottom quarter of the sorted stock list to get the long and short lists
	longShortAmount := int(len(alpacaClient.allStocks) / 4)
	fmt.Printf("%v", alpacaClient.allStocks)

	for i, stock := range alpacaClient.allStocks {
		if i < longShortAmount {
			alpacaClient.short.list = append(alpacaClient.short.list, stock.name)
		} else if i > (len(alpacaClient.allStocks) - 1 - longShortAmount) {
			alpacaClient.long.list = append(alpacaClient.long.list, stock.name)
		} else {
			continue
		}
	}

	// Determine amount to long/short based on total stock price of each bucket
	equity := 0.0
	positions, _ := alp.client.ListPositions()
	for _, position := range positions {
		rawVal, _ := position.MarketValue.Float64()
		equity += rawVal
	}

	alpacaClient.short.equityAmt = equity * 0.30
	alpacaClient.long.equityAmt = equity + alpacaClient.short.equityAmt

	getTPLong := make(chan float64)
	go alp.getTotalPrice(alpacaClient.long.list, getTPLong)
	longTotal := <-getTPLong

	getTPShort := make(chan float64)
	go alp.getTotalPrice(alpacaClient.short.list, getTPShort)
	shortTotal := <-getTPShort

	alpacaClient.long.qty = int(alpacaClient.long.equityAmt / longTotal)
	alpacaClient.short.qty = int(alpacaClient.short.equityAmt / shortTotal)
}

// Get the total price of the array of input stocks
func (alp alpacaClientContainer) getTotalPrice(arr []string, getTP chan float64) {
	totalPrice := 0.0
	for _, stock := range arr {
		numBars := 1
		bar, _ := alp.client.GetSymbolBars(stock, alpaca.ListBarParams{Timeframe: "minute", Limit: &numBars})
		totalPrice += float64(bar[0].Close)
	}
	getTP <- totalPrice
}

// Submit an order if quantity is above 0
func (alp alpacaClientContainer) submitOrder(qty int, symbol string, side string, cSubmitOrder chan error) {
	account, _ := alp.client.GetAccount()
	if qty > 0 {
		adjSide := alpaca.Side(side)
		_, err := alp.client.PlaceOrder(alpaca.PlaceOrderRequest{
			AccountID:   account.ID,
			AssetKey:    &symbol,
			Qty:         decimal.NewFromFloat(float64(qty)),
			Side:        adjSide,
			Type:        "market",
			TimeInForce: "day",
		})
		fmt.Println("Market order of " + strconv.Itoa(qty) + " " + symbol + ", " + side)
		cSubmitOrder <- err
	} else {
		fmt.Println("Quantity is 0, order of " + strconv.Itoa(qty) + " " + symbol + " " + side + " not completed")
		cSubmitOrder <- nil
	}
	return
}

// Submit a batch order that returns completed and uncompleted orders
func (alp alpacaClientContainer) sendBatchOrder(qty int, stocks []string, side string, cSendBO chan [2][]string) {
	var executed []string
	var incomplete []string
	for _, stock := range stocks {
		cIndexOf := make(chan int)
		go indexOf(alpacaClient.blacklist, stock, cIndexOf)
		index := <-cIndexOf
		if index > -1 {
			cSubmitOrder := make(chan error)
			go alp.submitOrder(qty, stock, side, cSubmitOrder)
			if <-cSubmitOrder != nil {
				incomplete = append(incomplete, stock)
				fmt.Printf("%v", incomplete)
			} else {
				executed = append(executed, stock)
				fmt.Printf("%v", executed)
			}
		}
	}
	cSendBO <- [2][]string{incomplete, executed}
}

// Get percent changes of the stock prices over the past 10 days
func (alp alpacaClientContainer) getPercentChanges(cGetPC chan bool) {
	length := 10
	for _, stock := range alpacaClient.allStocks {
		startTime, endTime := time.Unix(time.Now().Unix()-int64(length*60000), 0), time.Now()
		bars, err := alp.client.GetSymbolBars(stock.name, alpaca.ListBarParams{Timeframe: "day", StartDt: &startTime, EndDt: &endTime, Limit: &length})
		fmt.Printf("%v", err)
		fmt.Println()
		percentChange := bars[len(bars)-1].Close - bars[0].Close
		stock.pc = float64(percentChange)
	}
	cGetPC <- true
}

// Mechanism used to rank the stocks, the basis of the Long-Short Equity Strategy
func (alp alpacaClientContainer) rank(cRank chan bool) {
	// Ranks all stocks by percent change over the past 10 days (higher is better)
	cGetPC := make(chan bool)
	go alp.getPercentChanges(cGetPC)
	<-cGetPC

	// Sort the stocks in place by the percent change field (marked by pc)
	sort.Slice(alpacaClient.allStocks, func(i, j int) bool {
		return alpacaClient.allStocks[i].pc < alpacaClient.allStocks[j].pc
	})
	cRank <- true
}

func indexOf(arr []string, str string, cIndexOf chan int) {
	for i, elem := range arr {
		if elem == str {
			cIndexOf <- i
			return
		}
	}
	cIndexOf <- -1
	return
}
