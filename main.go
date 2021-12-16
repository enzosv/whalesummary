package main

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"flag"
	"fmt"
	"io"
	"io/ioutil"
	"log"
	"math"
	"net/http"
	"net/url"
	"os"
	"strconv"
	"strings"
	"time"

	"github.com/jackc/pgx/v4"
	"golang.org/x/text/language"
	"golang.org/x/text/message"
)

type WhaleAlertResponse struct {
	Result       string        `json:"result"`
	Message      string        `json:"message"`
	Cursor       string        `json:"cursor"`
	Count        int           `json:"count"`
	Transactions []Transaction `json:"transactions"`
}

type Transaction struct {
	Blockchain       string  `json:"blockchain"`
	Symbol           string  `json:"symbol"`
	ID               string  `json:"id"`
	TransactionType  string  `json:"transaction_type"`
	Hash             string  `json:"hash"`
	From             Wallet  `json:"from"`
	To               Wallet  `json:"to"`
	Timestamp        int     `json:"timestamp"`
	Amount           float64 `json:"amount"`
	AmountUsd        float64 `json:"amount_usd"`
	TransactionCount int     `json:"transaction_count"`
}

type Wallet struct {
	Address   string `json:"address"`
	Owner     string `json:"owner"`
	OwnerType string `json:"owner_type"`
}

type Config struct {
	Telegram    TelegramConfig    `json:"telegram"`
	WhaleAlert  WhaleAlertConfig  `json:"whale_alert"`
	StableCoins []string          `json:"stable_coins"`
	Remap       map[string]string `json:"remap"`
	LogDBURL    string            `json:"log_db_url"`
}

type TelegramConfig struct {
	BotID       string `json:"bot_id"`
	RecipientID string `json:"recipient_id"`
	LogID       string `json:"log_id"`
}
type WhaleAlertConfig struct {
	APIKey string `json:"api_key"`
	Min    string `json:"min"`   //minimum usd value of transaction
	Limit  int    `json:"limit"` //page limit
}

type TransactionType int

const (
	MINT TransactionType = iota
	BURN
	TRANSFER
)

func (t TransactionType) String() string {
	return [...]string{"mint", "burn", "transfer"}[t]
}

const TGURL = "https://api.telegram.org"
const WHALEURL = "https://api.whale-alert.io/v1/transactions"

func main() {
	configPath := flag.String("c", "config.json", "config file")
	interval := flag.Int64("interval", 48, "minutes between start and end if not provided")
	/*
		48 so cron is more convenient
		can't be 60 because whale alert complains about time range
		0,48 0,4,8,12,16,20 * * *
		36 1,5,9,13,17,21 * * *
		24 2,6,10,14,18,22 * * *
		12 3,7,11,15,19,23 * * *
	*/
	// rounded down to nearest minute
	start := flag.Int64("start", time.Now().Truncate(time.Minute).Unix()-*interval*60, "start time in unix seconds for fetching transactions")
	// 48 minutes after start
	// minus one second because whale alert end is inclusive
	end := flag.Int64("end", *start+*interval*60-1, "end time in unix seconds for fetching transactions")

	flag.Parse()
	config := parseConfig(*configPath)

	_, transactions, err := fetchTransactions(config.WhaleAlert, []Transaction{}, "", *start, *end, true)
	if err != nil {
		sendMessage(config.Telegram.BotID, config.Telegram.LogID, err.Error())
		// not returning to continue with successful requests if any
	}
	// sendMessage(config.Telegram.BotID, config.Telegram.LogID, fmt.Sprintf("[%d whale transactions](%s) from %s to %s",
	// 	len(transactions),
	// 	url,
	// 	time.Unix(*start, 0).Format("Jan 2 3:04:05PM"),
	// 	time.Unix(*end, 0).Format("3:04:05PM"),
	// ))
	if len(transactions) < 1 {
		return
	}
	logWhales(context.Background(), config.LogDBURL, transactions)
	supply, transfers, unhandled := summarizeTransactions(transactions, config.Remap)
	if len(unhandled) > 0 {
		sendMessage(config.Telegram.BotID, config.Telegram.LogID, "unhandled:\n"+strings.Join(unhandled, "\n"))
	}

	analysis := analyzeSummary(supply, transfers, config.StableCoins)
	sendMessage(config.Telegram.BotID, config.Telegram.RecipientID, analysis)
}

func parseConfig(path string) Config {
	configFile, err := os.Open(path)
	if err != nil {
		log.Fatal("Cannot open server configuration file: ", err)
	}
	defer configFile.Close()

	dec := json.NewDecoder(configFile)
	var config Config
	if err = dec.Decode(&config); errors.Is(err, io.EOF) {
		//do nothing
	} else if err != nil {
		log.Fatal("Cannot load server configuration file: ", err)
	}
	return config
}

func fetchTransactions(config WhaleAlertConfig, existing []Transaction, cursor string, start, end int64, retry bool) (string, []Transaction, error) {

	base, err := url.Parse(WHALEURL)
	if err != nil {
		return "", existing, err
	}
	params := url.Values{}
	params.Add("api_key", config.APIKey)
	params.Add("min_value", config.Min)
	params.Add("start", fmt.Sprintf("%d", start))
	params.Add("end", fmt.Sprintf("%d", end))
	params.Add("limit", strconv.Itoa(config.Limit))
	if cursor != "" {
		// for pagination
		params.Add("cursor", cursor)
	}
	base.RawQuery = params.Encode()
	request_url := base.String()
	res, err := http.Get(request_url)
	if err != nil {
		return request_url, existing, err
	}
	defer res.Body.Close()
	body, err := ioutil.ReadAll(res.Body)
	if err != nil {
		return request_url, existing, err
	}
	var response WhaleAlertResponse
	err = json.Unmarshal(body, &response)
	if err != nil {
		return request_url, existing, err
	}
	if response.Result != "success" {
		if retry {
			return fetchTransactions(config, existing, cursor, start, end, false)
		}
		return request_url, existing, fmt.Errorf(response.Message)
	}
	existing = append(existing, response.Transactions...)

	if response.Count >= config.Limit {
		return fetchTransactions(config, existing, response.Cursor, start, end, true)
	}
	return request_url, existing, nil
}

func summarizeTransactions(transactions []Transaction, tickermap map[string]string) (map[string]float64, map[string]float64, []string) {
	transfers := map[string]float64{}
	supply := map[string]float64{}
	var unhandled []string

	for _, transaction := range transactions {
		// TODO: side effect log addresses
		symbol := transaction.Symbol
		// remap symbol like pax is actually usdp
		if value, ok := tickermap[symbol]; ok {
			symbol = value
		}
		if transaction.TransactionType == MINT.String() {
			supply[symbol] += transaction.AmountUsd
			continue
		}
		if transaction.TransactionType == BURN.String() {
			supply[symbol] -= transaction.AmountUsd
			continue
		}
		if transaction.TransactionType != TRANSFER.String() {
			unhandled = append(unhandled, fmt.Sprintf("  %s:  %s (%s) -> %s (%s)",
				transaction.TransactionType,
				transaction.From.OwnerType, transaction.From.Owner,
				transaction.To.OwnerType, transaction.To.Owner))
			continue
		}
		if transaction.From.OwnerType == transaction.To.OwnerType {
			// ignore internal
			continue
		}
		if transaction.From.OwnerType == "exchange" {
			// exchange outflow
			transfers[symbol] -= transaction.AmountUsd
			continue
		}
		if transaction.To.OwnerType == "exchange" {
			// exchange inflow
			transfers[symbol] += transaction.AmountUsd
			continue
		}
		// everything else is ignored
		// TODO: handle others
	}
	return supply, transfers, unhandled

}

func analyzeSummary(supply, transfers map[string]float64, stablecoins []string) string {
	p := message.NewPrinter(language.English)
	var msg []string
	// TODO: Separate function to process supply
	var mints []string
	var burns []string
	for key, value := range supply {
		abs := math.Abs(value)
		if abs < 1000000 {
			// sum of mint and burn might be insignificant. ignore
			continue
		}
		m := p.Sprintf("  `%-5s`: $%.0f", strings.ToUpper(key), abs)
		if value < 0 {
			if isStableCoin(key, stablecoins) {
				// burning of stable coin suggets conversion into fiat. bearish
				m += " (bear)"
			} else {
				// burning of crypto means less supply and higher price. bullish
				m += " (bull)"
			}
			burns = append(burns, m)
		} else {
			if isStableCoin(key, stablecoins) {
				//minting of new stable coin suggests conversion from fiat. bullish
				m += " (bull)"
			} else {
				// minting of new crypto means more supply and lower price. bearish
				m += " (bear)"
			}
			mints = append(mints, m)
		}
	}
	if len(mints) > 0 {
		msg = append(msg, "Mints:")
		msg = append(msg, mints...)
	}
	if len(burns) > 0 {
		msg = append(msg, "Burns:")
		msg = append(msg, burns...)
	}

	// TODO: separate function to process transfers
	var withdraws []string
	var deposits []string
	for key, value := range transfers {
		abs := math.Abs(value)
		if abs < 1000000 {
			// sum of inflow and outflow might be insignificant. ignore
			continue
		}
		m := p.Sprintf("  `%-5s`: $%.0f", strings.ToUpper(key), abs)
		if value < 0 {
			// outflow
			if isStableCoin(key, stablecoins) {
				// outlfow of stable coin suggests whales aren't buying. bearish
				m += " (bear)"
			} else {
				// outflow of crypto suggests whales are going to hodl. bullish
				m += " (bull)"
			}
			withdraws = append(withdraws, m)
		} else if value > 0 {
			// inflow
			if isStableCoin(key, stablecoins) {
				// inflow of stable coin suggests whales are looking to buy. bullish
				m += " (bull)"
			} else {
				// inflow of crypto suggests whales are looking to sell. bearish
				m += " (bear)"
			}
			deposits = append(deposits, m)
		}
	}
	if len(deposits) > 0 {
		msg = append(msg, "Exchange Inflow:")
		msg = append(msg, deposits...)
	}
	if len(withdraws) > 0 {
		msg = append(msg, "Exchange Outflow:")
		msg = append(msg, withdraws...)
	}
	return strings.Join(msg, "\n")
}

func isStableCoin(symbol string, stablecoins []string) bool {
	lowercaseSymbol := strings.ToLower(symbol)
	for _, ticker := range stablecoins {
		// is this better than strings.EqualFold(ticker, symbol)
		if strings.ToLower(ticker) == lowercaseSymbol {
			return true
		}
	}
	return false
}

func constructPayload(chatID, message string) (*bytes.Reader, error) {
	payload := map[string]interface{}{}
	payload["chat_id"] = chatID
	payload["text"] = message
	payload["parse_mode"] = "markdown"

	jsonValue, err := json.Marshal(payload)
	return bytes.NewReader(jsonValue), err
}

func sendMessage(bot, chatID, message string) error {
	payload, err := constructPayload(chatID, message)
	if err != nil {
		fmt.Println(err)
		return err
	}
	req, err := http.NewRequest("POST", fmt.Sprintf("%s/bot%s/sendMessage", TGURL, bot), payload)
	if err != nil {
		fmt.Println(err)
		return err
	}
	req.Header.Set("Content-Type", "application/json")
	res, err := http.DefaultClient.Do(req)
	if err != nil {
		fmt.Println(err)
		return err
	}
	defer res.Body.Close()
	body, err := ioutil.ReadAll(res.Body)
	if err != nil {
		fmt.Println(err)
		return err
	}
	fmt.Println(string(body))
	return nil
}

func logWhales(ctx context.Context, pgurl string, transactions []Transaction) {
	query := `
		INSERT INTO whales
		(blockchain, address, owner, owner_type)
		VALUES ($1, $2, NULLIF($3, ''), $4)
		ON CONFLICT ON CONSTRAINT ux_blockchain_address DO UPDATE SET
			owner = NULLIF($3, ''),
			owner_type = $4;
	`
	conn, err := pgx.Connect(ctx, pgurl)
	if err != nil {
		return
	}
	defer conn.Close(ctx)
	for _, transaction := range transactions {
		conn.Exec(ctx, query, transaction.Blockchain, transaction.From.Address, transaction.From.Owner, transaction.From.OwnerType)
		conn.Exec(ctx, query, transaction.Blockchain, transaction.To.Address, transaction.To.Owner, transaction.To.OwnerType)
	}
}
