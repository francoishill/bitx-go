/*
Package streaming implements a client for the Luno Streaming API.

Example:

	c, err := streaming.Dial(keyID, keySecret, "XBTZAR")
	if err != nil {
		log.Fatal(err)
	}
	defer c.Close()

	for {
		seq, bids, asks := c.OrderBookSnapshot()
		log.Printf("%d: %v %v\n", seq, bids[0], asks[0])
		time.Sleep(time.Minute)
	}
*/
package streaming

import (
	"encoding/json"
	"errors"
	"flag"
	"fmt"
	"log"
	"math/rand"
	"sort"
	"sync"
	"time"

	"github.com/bitx/bitx-go"
	"golang.org/x/net/websocket"
)

func convertOrders(ol []*order) (map[string]order, error) {
	r := make(map[string]order)
	for _, o := range ol {
		r[o.ID] = *o
	}
	return r, nil
}

type orderList []bitx.OrderBookEntry

func (ol orderList) Less(i, j int) bool {
	return ol[i].Price < ol[j].Price
}
func (ol orderList) Swap(i, j int) {
	ol[i], ol[j] = ol[j], ol[i]
}
func (ol orderList) Len() int {
	return len(ol)
}

type orderListGroup []OrderBookEntryGroup

func (ol orderListGroup) Less(i, j int) bool {
	return ol[i].Price < ol[j].Price
}
func (ol orderListGroup) Swap(i, j int) {
	ol[i], ol[j] = ol[j], ol[i]
}
func (ol orderListGroup) Len() int {
	return len(ol)
}

func flatten(m map[string]order, reverse bool) []bitx.OrderBookEntry {
	var ol []bitx.OrderBookEntry
	for _, o := range m {
		ol = append(ol, bitx.OrderBookEntry{
			Price:  o.Price,
			Volume: o.Volume,
		})
	}
	if reverse {
		sort.Sort(sort.Reverse(orderList(ol)))
	} else {
		sort.Sort(orderList(ol))
	}
	return ol
}

type OrderBookEntryGroup struct {
	Price, Volume float64
	Count         int64
}

func flattenGroupByPriceSumVolume(m map[string]order, reverse bool) []OrderBookEntryGroup {
	priceVolMap := make(map[int64]float64)
	priceCountMap := make(map[int64]int64)
	for _, o := range m {
		priceInt := int64(o.Price)
		existingVolSum, ok := priceVolMap[priceInt]
		if !ok {
			priceVolMap[priceInt] = o.Volume
			priceCountMap[priceInt] = 1
			continue
		}

		priceVolMap[priceInt] = existingVolSum + o.Volume
		priceCountMap[priceInt]++
	}

	var ol []OrderBookEntryGroup
	for price, vol := range priceVolMap {
		ol = append(ol, OrderBookEntryGroup{
			Price:  float64(price),
			Volume: vol,
			Count:  priceCountMap[price],
		})
	}
	if reverse {
		sort.Sort(sort.Reverse(orderListGroup(ol)))
	} else {
		sort.Sort(orderListGroup(ol))
	}
	return ol
}

type Conn struct {
	keyID, keySecret string
	pair             string

	ws     *websocket.Conn
	closed bool

	seq  int64
	bids map[string]order
	asks map[string]order

	lastMessage time.Time

	mu sync.Mutex
}

// Dial initiates a connection to the streaming service and starts processing
// data for the given market pair.
// The connection will automatically reconnect on error.
func Dial(keyID, keySecret, pair string) (*Conn, error) {
	c := &Conn{
		keyID:     keyID,
		keySecret: keySecret,
		pair:      pair,
	}
	go c.manageForever()
	return c, nil
}

var wsHost = flag.String(
	"luno_websocket_host", "wss://ws.luno.com", "Luno API websocket host")

func (c *Conn) manageForever() {
	attempts := 0
	var lastAttempt time.Time
	for {
		c.mu.Lock()
		closed := c.closed
		c.mu.Unlock()
		if closed {
			return
		}

		lastAttempt = time.Now()
		attempts++
		if err := c.connect(); err != nil {
			log.Printf("bitx-go/streaming: Connection error key=%s pair=%s: %v", c.keyID, c.pair, err)
		}

		if time.Now().Sub(lastAttempt) > time.Hour {
			attempts = 0
		}
		if attempts > 5 {
			attempts = 5
		}
		wait := 5
		for i := 0; i < attempts; i++ {
			wait = 2 * wait
		}
		wait = wait + rand.Intn(wait)
		dt := time.Duration(wait) * time.Second
		log.Printf("bitx-go/streaming: Waiting %s before reconnecting", dt)
		time.Sleep(dt)
	}
}

func (c *Conn) connect() error {
	url := *wsHost + "/api/1/stream/" + c.pair
	ws, err := websocket.Dial(url, "", "http://localhost/")
	if err != nil {
		return err
	}
	defer func() {
		ws.Close()
		c.mu.Lock()
		c.ws = nil
		c.seq = 0
		c.bids = nil
		c.asks = nil
		c.mu.Unlock()
	}()

	c.mu.Lock()
	if c.closed {
		c.mu.Unlock()
		return nil
	} else {
		c.ws = ws
		c.mu.Unlock()
	}

	cred := credentials{c.keyID, c.keySecret}
	if err := websocket.JSON.Send(ws, cred); err != nil {
		return err
	}

	log.Printf("bitx-go/streaming: Connection established key=%s pair=%s", c.keyID, c.pair)

	go sendPings(ws)

	for {
		var data []byte
		err := websocket.Message.Receive(c.ws, &data)
		if err != nil {
			return err
		}

		if string(data) == "\"\"" {
			c.receivedPing()
			continue
		}

		var ob orderBook
		if err := json.Unmarshal(data, &ob); err != nil {
			return err
		}
		if ob.Asks != nil || ob.Bids != nil {
			// Received an order book.
			if err := c.receivedOrderBook(ob); err != nil {
				return err
			}
			continue
		}

		var u update
		if err := json.Unmarshal(data, &u); err != nil {
			return err
		}
		if err := c.receivedUpdate(u); err != nil {
			return err
		}
	}
}

func sendPings(ws *websocket.Conn) {
	defer ws.Close()
	for {
		if err := websocket.Message.Send(ws, ""); err != nil {
			return
		}
		time.Sleep(time.Minute)
	}
}

func (c *Conn) receivedPing() {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.lastMessage = time.Now()
}

func (c *Conn) receivedOrderBook(ob orderBook) error {
	bids, err := convertOrders(ob.Bids)
	if err != nil {
		return err
	}

	asks, err := convertOrders(ob.Asks)
	if err != nil {
		return err
	}

	c.mu.Lock()
	defer c.mu.Unlock()

	c.lastMessage = time.Now()
	c.seq = ob.Sequence
	c.bids = bids
	c.asks = asks
	return nil
}

func (c *Conn) receivedUpdate(u update) error {
	c.mu.Lock()
	defer c.mu.Unlock()

	if c.seq == 0 {
		// State not initialized so we can't update it.
		return nil
	}

	if u.Sequence <= c.seq {
		// Old update. We can just discard it.
		return nil
	}
	if u.Sequence != c.seq+1 {
		return errors.New("update received out of sequence")
	}

	// Process trades
	for _, t := range u.TradeUpdates {
		if err := c.processTrade(*t); err != nil {
			return err
		}
	}

	// Process create
	if u.CreateUpdate != nil {
		if err := c.processCreate(*u.CreateUpdate); err != nil {
			return err
		}
	}

	// Process delete
	if u.DeleteUpdate != nil {
		if err := c.processDelete(*u.DeleteUpdate); err != nil {
			return err
		}
	}

	c.lastMessage = time.Now()
	c.seq = u.Sequence

	return nil
}

// addD8 adds the two values and rounds the result to the nearest 8 decimal
// places.
func addD8(a, b float64) float64 {
	s := a + b
	return float64(int64(s*1e8+0.5)) / 1e8
}

func decTrade(m map[string]order, id string, base float64) (bool, error) {
	o, ok := m[id]
	if !ok {
		return false, nil
	}

	o.Volume = addD8(o.Volume, -base)

	if o.Volume < 0 {
		return false, fmt.Errorf("negative volume: %f", o.Volume)
	}

	if o.Volume == 0 {
		delete(m, id)
	} else {
		m[id] = o
	}
	return true, nil
}

func (c *Conn) processTrade(t tradeUpdate) error {
	if t.Base <= 0 {
		return errors.New("nonpositive trade")
	}

	ok, err := decTrade(c.bids, t.OrderID, t.Base)
	if err != nil {
		return err
	}
	if ok {
		return nil
	}

	ok, err = decTrade(c.asks, t.OrderID, t.Base)
	if err != nil {
		return err
	}
	if ok {
		return nil
	}

	return errors.New("trade for unknown order")
}

func (c *Conn) processCreate(u createUpdate) error {
	o := order{
		ID:     u.OrderID,
		Price:  u.Price,
		Volume: u.Volume,
	}

	if u.Type == string(bitx.BID) {
		c.bids[o.ID] = o
	} else if u.Type == string(bitx.ASK) {
		c.asks[o.ID] = o
	} else {
		return errors.New("unknown order type")
	}

	return nil
}

func (c *Conn) processDelete(u deleteUpdate) error {
	delete(c.bids, u.OrderID)
	delete(c.asks, u.OrderID)
	return nil
}

// OrderBookSnapshot returns the latest order book.
func (c *Conn) OrderBookSnapshot() (int64, []bitx.OrderBookEntry, []bitx.OrderBookEntry) {
	c.mu.Lock()
	defer c.mu.Unlock()

	bids := flatten(c.bids, true)
	asks := flatten(c.asks, false)
	return c.seq, bids, asks
}

// OrderBookSnapshotGroupByPriceSumVolume returns the latest order book with same prices grouped and summing the volumes.
func (c *Conn) OrderBookSnapshotGroupByPriceSumVolume() (int64, []OrderBookEntryGroup, []OrderBookEntryGroup) {
	c.mu.Lock()
	defer c.mu.Unlock()

	bids := flattenGroupByPriceSumVolume(c.bids, true)
	asks := flattenGroupByPriceSumVolume(c.asks, false)
	return c.seq, bids, asks
}

// Close the connection.
func (c *Conn) Close() {
	c.mu.Lock()
	defer c.mu.Unlock()

	c.closed = true
	if c.ws != nil {
		c.ws.Close()
	}
}
