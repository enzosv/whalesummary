# What this is
Tool to summarize and report large exchange inflows, exchange outflows, mints, and burns.<br>
Data is sourced from [whale-alert](https://whale-alert.io/) and reported via a [telegram channel](https://t.me/whalesummary).

## Limitations
1. Only reads transactions reported by whale alert
  * Limited to blockchains and tokens they [monitor](https://api.whale-alert.io/v1/status)
2. Only reads 50 minutes worth of transaction history
3. Only reads transactions >= $500,000
4. Only considers transfers to and from exchanges
  * Does not consider transfers from one exchange to another
5. Only a summary
  * Go to [whale-alert](https://whale-alert.io/) or check with the blockchain for more detail
6. List of stable coins is manual. It may be wrong. It is incomplete.

## How bullish or bearish is considered
1. Minting
  * Minting of new stable coin suggests conversion of fiat. *Bullish*.
  * Minting of new crypto means an increase in supply and a decrease in price. *Bearish*.
2. Burning
  * Burning of stable coin suggests conversion into fiat. *Bearish*.
  * Burning of crypto means a decrease in supply and an increase in price. *Bullish*.
3. Exchange Inflows
  * Transfer of stable coin into exchanges suggests an intent to buy crypto. *Bullish*.
  * Transfer of crypto into exchanges suggests an intent to sell. *Bearish*.
4. Exchange Outflows
  * Transfer of stable coin out of exchanges suggests buying has stopped. *Bearish*.
  * Transfer of crypto out of exchanges suggests selling has stopped. *Bullish*.
### Note
Be aware that whales are aware that we are aware and so on.<br>
These are not guarantees nor are they financial advice. Just my opinion.

# Development
## Requirements
1. go
2. config.json file. See sample_config.json.

## Build and run
```
go get -d
go build
./whalesummary
```

Tips are appreciated. 0xBa2306a4e2AadF2C3A6084f88045EBed0E842bF9