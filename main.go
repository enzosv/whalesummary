package main

import (
	"bytes"
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
	Telegram    TelegramConfig   `json:"telegram"`
	WhaleAlert  WhaleAlertConfig `json:"whale_alert"`
	StableCoins []string         `json:"stable_coins"`
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

const TGURL = "https://api.telegram.org"
const WHALEURL = "https://api.whale-alert.io/v1/transactions"

func main() {
	configPath := flag.String("c", "config.json", "config file")
	start := flag.Int64("start", time.Now().Truncate(10*time.Minute).Add(time.Minute*-50).Unix(), "start time in unix seconds for fetching transactions")
	end := flag.Int64("end", *start+60*50-1, "end time in unix seconds for fetching transactions")
	flag.Parse()
	config := parseConfig(*configPath)

	url, transactions, err := fetchTransactions(config.WhaleAlert, []Transaction{}, "", *start, *end, true)
	if err != nil {
		sendMessage(config.Telegram.BotID, config.Telegram.LogID, err.Error())
	}
	sendMessage(config.Telegram.BotID, config.Telegram.LogID, fmt.Sprintf("%d whale transactions from %s to %s\n%s",
		len(transactions),
		time.Unix(*start, 0).Format("Jan 2 3:04PM"),
		time.Unix(*end, 0).Format("3:04PM"),
		url,
	))
	if len(transactions) < 1 {
		return
	}

	supply, transfers, unhandled := summarizeTransactions(transactions)
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

func summarizeTransactions(transactions []Transaction) (map[string]float64, map[string]float64, []string) {
	transfers := map[string]float64{}
	supply := map[string]float64{}
	var unhandled []string

	for _, transaction := range transactions {
		if transaction.TransactionType == "mint" {
			supply[transaction.Symbol] += transaction.AmountUsd
			continue
		}
		if transaction.TransactionType == "burn" {
			supply[transaction.Symbol] -= transaction.AmountUsd
			continue
		}
		if transaction.TransactionType != "transfer" {
			unhandled = append(unhandled, fmt.Sprintf("  %s:  %s (%s) -> %s (%s)",
				transaction.TransactionType,
				transaction.From.OwnerType, transaction.From.Owner,
				transaction.To.OwnerType, transaction.To.Owner))
			continue
		}
		if transaction.From.OwnerType == "exchange" && transaction.To.OwnerType != "exchange" {
			// exchange outflow
			transfers[transaction.Symbol] -= transaction.AmountUsd
			continue
		}
		if transaction.From.OwnerType != "exchange" && transaction.To.OwnerType == "exchange" {
			// exchange inflow
			transfers[transaction.Symbol] += transaction.AmountUsd
			continue
		}
		if transaction.From.OwnerType == "unknown" && transaction.To.OwnerType == "unknown" {
			//ignore p2p
			continue
		}
		if transaction.From.OwnerType == "exchange" && transaction.To.OwnerType == "exchange" {
			//ignore b2b
			continue
		}
		if transaction.From.Owner == transaction.To.Owner {
			//ignore internal
			continue
		}
		if transaction.From.OwnerType == "other" && transaction.To.OwnerType == "other" {
			//ignore p2p
			continue
		}
		if transaction.From.OwnerType == "unknown" && transaction.To.OwnerType == "other" {
			//ignore p2p
			continue
		}
		if transaction.From.OwnerType == "other" && transaction.To.OwnerType == "unknown" {
			//ignore p2p
			continue
		}
		// TODO: handle others
		unhandled = append(unhandled, fmt.Sprintf("  %s:  %s (%s) -> %s (%s)",
			transaction.TransactionType,
			transaction.From.OwnerType, transaction.From.Owner,
			transaction.To.OwnerType, transaction.To.Owner))
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
		m := p.Sprintf("  `%-5s`: $%.0f", strings.ToUpper(key), math.Abs(value))
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
		m := p.Sprintf("  `%-5s`: $%.0f", strings.ToUpper(key), math.Abs(value))
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
	for _, ticker := range stablecoins {
		if ticker == symbol {
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
