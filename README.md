# Flow Source for Stellar RPC

This source plugin for the Flow framework connects to a Stellar RPC endpoint to fetch ledger data. It continuously polls for new ledgers and forwards them to downstream processors.

## Features

- Connects to a Stellar RPC endpoint
- Supports API key authentication
- Configurable polling interval
- Can start from a specific ledger or the latest ledger
- Forwards ledger data to downstream processors

## Usage

### Building the Plugin

```bash
go build -buildmode=plugin -o flow-source-stellar-rpc.so
```

### Pipeline Configuration

Add this source to your Flow pipeline configuration:

```yaml
pipelines:
  SoroswapPipeline:
    source:
      type: "flow/source/stellar-rpc"
      config:
        rpc_endpoint: "https://rpc-pubnet.nodeswithobsrvr.co"
        api_key: "your-api-key"  # Optional
        poll_interval: 5  # Optional, in seconds, defaults to 5
        start_ledger: 56075000  # Optional, defaults to latest ledger
    processors:
      - type: "flow/processor/contract-events"
        config:
          network_passphrase: "Public Global Stellar Network ; September 2015"
    consumers:
      - type: "flow/consumer/zeromq"
        config:
          address: "tcp://127.0.0.1:5555"
```

## Configuration Options

| Option | Type | Required | Description |
|--------|------|----------|-------------|
| `rpc_endpoint` | string | Yes | URL of the Stellar RPC endpoint |
| `api_key` | string | No | API key for authentication |
| `poll_interval` | number | No | Interval in seconds between ledger polls (default: 5) |
| `start_ledger` | number | No | Ledger sequence to start from (default: latest) |

## Message Format

The plugin forwards messages to processors with the following structure:

```go
pluginapi.Message{
    Payload:   encodedHeader,  // Base64-encoded ledger header
    Timestamp: time.Unix(int64(ledgerInfo.LedgerCloseTime), 0),
    Metadata: map[string]interface{}{
        "ledger_sequence": ledgerSequence,
        "source":          "stellar-rpc",
    },
}
```

## Integration with Flow

This source plugin is designed to work with the Flow pipeline system and can be chained with other processors and consumers. It's particularly useful for:

1. Real-time monitoring of the Stellar blockchain
2. Processing contract events as they occur
3. Building applications that need up-to-date ledger data

## Error Handling

The plugin includes robust error handling:

- Connection errors are logged and retried
- Invalid ledger data is reported
- Missing ledgers are detected

## Dependencies

- github.com/stellar/go
- github.com/stellar/stellar-rpc
- github.com/withObsrvr/pluginapi 