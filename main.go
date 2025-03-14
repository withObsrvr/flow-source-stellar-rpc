package main

import (
	"context"
	"encoding/base64"
	"encoding/json"
	"fmt"
	"log"
	"net/http"
	"sync"
	"time"

	"github.com/pkg/errors"
	"github.com/stellar/go/xdr"
	"github.com/stellar/stellar-rpc/client"
	"github.com/stellar/stellar-rpc/protocol"
	"github.com/withObsrvr/pluginapi"
)

type RPCLedgerSource struct {
	rpcClient     *client.Client
	ledgerMu      sync.Mutex
	processors    []pluginapi.Processor
	endpoint      string
	apiKey        string
	pollInterval  time.Duration
	currentLedger uint32
	format        string // Format to use when retrieving ledgers: "base64" or "json"
	stopCh        chan struct{}
	wg            sync.WaitGroup
}

func (src *RPCLedgerSource) Name() string {
	return "flow/source/stellar-rpc"
}

func (src *RPCLedgerSource) Version() string {
	return "1.0.0"
}

func (src *RPCLedgerSource) Type() pluginapi.PluginType {
	return pluginapi.SourcePlugin
}

func (src *RPCLedgerSource) Initialize(config map[string]interface{}) error {
	endpoint, ok := config["rpc_endpoint"].(string)
	if !ok {
		return fmt.Errorf("rpc_endpoint configuration is required")
	}
	src.endpoint = endpoint

	apiKey, _ := config["api_key"].(string)
	src.apiKey = apiKey

	// Set default poll interval to 5 seconds if not specified
	pollInterval := 5 * time.Second
	if interval, ok := config["poll_interval"].(float64); ok {
		pollInterval = time.Duration(interval) * time.Second
	}
	src.pollInterval = pollInterval

	// Set starting ledger if specified
	if startLedger, ok := config["start_ledger"].(float64); ok {
		src.currentLedger = uint32(startLedger)
	}

	// Set format if specified, default to "base64"
	src.format = "base64"
	if format, ok := config["format"].(string); ok {
		if format == "json" || format == "base64" {
			src.format = format
		} else {
			log.Printf("Warning: Invalid format '%s', using default 'base64'", format)
		}
	}
	log.Printf("Using format: %s", src.format)

	src.stopCh = make(chan struct{})

	httpClient := &http.Client{}
	if apiKey != "" {
		httpClient.Transport = &transportWithAPIKey{
			apiKey: apiKey,
			rt:     http.DefaultTransport,
		}
	}

	src.rpcClient = client.NewClient(endpoint, httpClient)
	log.Printf("RPCLedgerSource initialized with endpoint: %s, poll interval: %s", endpoint, pollInterval)
	return nil
}

type transportWithAPIKey struct {
	apiKey string
	rt     http.RoundTripper
}

func (t *transportWithAPIKey) RoundTrip(req *http.Request) (*http.Response, error) {
	req.Header.Set("Authorization", "Api-Key "+t.apiKey)
	return t.rt.RoundTrip(req)
}

// Subscribe implements the Source interface
func (src *RPCLedgerSource) Subscribe(processor pluginapi.Processor) {
	src.ledgerMu.Lock()
	defer src.ledgerMu.Unlock()
	src.processors = append(src.processors, processor)
	log.Printf("Processor %s subscribed to RPCLedgerSource", processor.Name())
}

// Start implements the Source interface
func (src *RPCLedgerSource) Start(ctx context.Context) error {
	log.Printf("Starting RPCLedgerSource with endpoint: %s", src.endpoint)

	// If no current ledger is set, get the latest ledger
	if src.currentLedger == 0 {
		// First try GetLatestLedger
		latestResp, err := src.rpcClient.GetLatestLedger(ctx)
		if err == nil {
			// Successfully got latest ledger
			src.currentLedger = latestResp.Sequence
			log.Printf("Starting from latest ledger: %d", src.currentLedger)
		} else {
			log.Printf("Failed to get latest ledger with GetLatestLedger: %v", err)

			// Try GetLedgers as fallback
			ledgersResp, ledgersErr := src.rpcClient.GetLedgers(ctx, protocol.GetLedgersRequest{
				Pagination: &protocol.LedgerPaginationOptions{
					Limit: 1,
				},
			})

			if ledgersErr == nil && len(ledgersResp.Ledgers) > 0 {
				src.currentLedger = ledgersResp.Ledgers[0].Sequence
				log.Printf("Starting from ledger: %d (from GetLedgers fallback)", src.currentLedger)
			} else {
				// If all else fails, use a reasonable default or the configured start_ledger
				if src.currentLedger == 0 {
					// Use a reasonable default
					src.currentLedger = 56117845 // Use the ledger from the config as default
					log.Printf("Using default ledger: %d (after all methods failed)", src.currentLedger)
				}
			}
		}
	} else {
		log.Printf("Starting from specified ledger: %d", src.currentLedger)
	}

	src.wg.Add(1)
	go src.pollLedgers(ctx)

	return nil
}

// Stop implements the Source interface
func (src *RPCLedgerSource) Stop() error {
	log.Printf("Stopping RPCLedgerSource")
	close(src.stopCh)
	src.wg.Wait()
	return nil
}

// pollLedgers continuously polls for new ledgers
func (src *RPCLedgerSource) pollLedgers(ctx context.Context) {
	defer src.wg.Done()

	ticker := time.NewTicker(src.pollInterval)
	defer ticker.Stop()

	for {
		select {
		case <-src.stopCh:
			log.Printf("Ledger polling stopped")
			return
		case <-ticker.C:
			if err := src.fetchAndProcessLedger(ctx); err != nil {
				log.Printf("Error processing ledger: %v", err)
			}
		}
	}
}

// fetchAndProcessLedger fetches a ledger and sends it to processors
func (src *RPCLedgerSource) fetchAndProcessLedger(ctx context.Context) error {
	// Fetch the ledger using GetLedgers with pagination
	resp, err := src.rpcClient.GetLedgers(ctx, protocol.GetLedgersRequest{
		StartLedger: src.currentLedger,
		Pagination: &protocol.LedgerPaginationOptions{
			Limit: 1,
		},
		// Use the format specified in the config
		Format: src.format,
	})
	if err != nil {
		return fmt.Errorf("failed to fetch ledger %d: %w", src.currentLedger, err)
	}

	// Check if we got any ledgers
	if len(resp.Ledgers) == 0 {
		// This could happen if we're at the latest ledger and no new ones are available
		log.Printf("No ledgers available starting from %d, waiting for next poll", src.currentLedger)
		return nil
	}

	// Process the ledger
	ledger := resp.Ledgers[0]
	if ledger.Sequence != src.currentLedger {
		log.Printf("Warning: Requested ledger %d but got %d", src.currentLedger, ledger.Sequence)
	}

	log.Printf("Processing ledger %d with hash %s", ledger.Sequence, ledger.Hash)

	var payload interface{}
	var format string

	// Process based on the configured format
	if src.format == "base64" {
		// Try to convert the ledger to XDR format
		ledgerXDR, err := src.convertToXDR(ledger)
		if err != nil {
			log.Printf("Warning: Could not convert ledger to XDR format: %v", err)
			// If we can't convert to XDR, we can't send to contract-events processor
			return fmt.Errorf("failed to convert ledger to XDR format: %w", err)
		}
		// Dereference the pointer to get the actual LedgerCloseMeta value
		payload = *ledgerXDR
		format = "xdr"
		log.Printf("Using XDR format for ledger %d", ledger.Sequence)
	} else {
		// Using JSON format
		// Check if we have JSON metadata directly
		if ledger.LedgerMetadataJSON != nil && len(ledger.LedgerMetadataJSON) > 0 {
			// Parse the JSON metadata
			var parsedMetadata map[string]interface{}
			if err := json.Unmarshal(ledger.LedgerMetadataJSON, &parsedMetadata); err != nil {
				log.Printf("Error parsing JSON metadata: %v", err)
				// Fall back to raw metadata
				payload = ledger.LedgerMetadataJSON
				format = "raw_json"
			} else {
				// Successfully parsed JSON metadata
				payload = parsedMetadata
				format = "json"
				log.Printf("Using parsed JSON metadata for ledger %d", ledger.Sequence)
			}
		} else if ledger.LedgerMetadata != "" {
			// Try to parse the metadata as JSON
			var parsedMetadata map[string]interface{}
			if err := json.Unmarshal([]byte(ledger.LedgerMetadata), &parsedMetadata); err != nil {
				log.Printf("Error parsing ledger metadata as JSON: %v", err)
				// Fall back to raw metadata
				payload = []byte(ledger.LedgerMetadata)
				format = "raw"
			} else {
				// Successfully parsed JSON
				payload = parsedMetadata
				format = "json"
				log.Printf("Using parsed JSON from metadata for ledger %d", ledger.Sequence)
			}
		} else {
			// No metadata available
			log.Printf("No metadata available for ledger %d", ledger.Sequence)
			payload = map[string]interface{}{} // Empty map
			format = "empty"
		}

		// For JSON format, create a properly formatted contract events structure
		// that processors like contract-events can understand
		contractEvents := map[string]interface{}{
			"ledger_sequence": ledger.Sequence,
			"ledger_hash":     ledger.Hash,
			"events":          []interface{}{}, // Empty events array by default
		}

		// If we have parsed metadata, try to extract events
		if metadataMap, ok := payload.(map[string]interface{}); ok {
			// Try to extract events from the metadata
			// The exact path to events depends on the structure of the metadata
			if txs, ok := metadataMap["transactions"].([]interface{}); ok {
				for _, tx := range txs {
					if txMap, ok := tx.(map[string]interface{}); ok {
						if events, ok := txMap["events"].([]interface{}); ok {
							contractEvents["events"] = append(contractEvents["events"].([]interface{}), events...)
						}
					}
				}
			}
		}

		// Use the formatted contract events as payload
		payload = contractEvents
		log.Printf("Created formatted contract events structure for ledger %d", ledger.Sequence)
	}

	msg := pluginapi.Message{
		Payload:   payload,
		Timestamp: time.Unix(ledger.LedgerCloseTime, 0),
		Metadata: map[string]interface{}{
			"ledger_sequence": ledger.Sequence,
			"ledger_hash":     ledger.Hash,
			"source":          "stellar-rpc",
			"format":          format,
		},
	}

	// Process through each processor in sequence
	return src.processLedgerWithProcessors(ctx, ledger, msg)
}

// convertToXDR attempts to convert a ledger from RPC format to XDR format
func (src *RPCLedgerSource) convertToXDR(ledger protocol.LedgerInfo) (*xdr.LedgerCloseMeta, error) {
	// Check if we have the XDR data directly
	if ledger.LedgerMetadata != "" {
		var ledgerCloseMeta xdr.LedgerCloseMeta

		// Try to decode the base64-encoded XDR data
		xdrBytes, err := base64.StdEncoding.DecodeString(ledger.LedgerMetadata)
		if err != nil {
			return nil, fmt.Errorf("failed to decode XDR data: %w", err)
		}

		// Unmarshal the XDR data
		if err := xdr.SafeUnmarshal(xdrBytes, &ledgerCloseMeta); err != nil {
			return nil, fmt.Errorf("failed to unmarshal XDR data: %w", err)
		}

		return &ledgerCloseMeta, nil
	}

	// If we don't have XDR data directly, we would need to construct it from the JSON data
	// This is a complex process and would require detailed knowledge of the Stellar XDR structures
	return nil, fmt.Errorf("XDR conversion from JSON not implemented")
}

// processLedgerWithProcessors processes the ledger through all registered processors
func (src *RPCLedgerSource) processLedgerWithProcessors(ctx context.Context, ledger protocol.LedgerInfo, msg pluginapi.Message) error {
	sequence := ledger.Sequence
	log.Printf("Starting to process ledger %d through processors", sequence)

	// Get a copy of the processors to avoid holding the lock during processing
	src.ledgerMu.Lock()
	processors := src.processors
	src.ledgerMu.Unlock()

	// Check if we have any processors
	if len(processors) == 0 {
		log.Printf("Warning: No processors registered for ledger %d", sequence)
		return nil
	}

	// Process through each processor in sequence
	for i, proc := range processors {
		select {
		case <-ctx.Done():
			return ctx.Err()
		default:
			procStart := time.Now()

			// Add processor-specific context
			processorCtx := context.WithValue(ctx, "processor_index", i)
			processorCtx = context.WithValue(processorCtx, "processor_type", fmt.Sprintf("%T", proc))

			if err := proc.Process(processorCtx, msg); err != nil {
				log.Printf("Error in processor %d (%T) for ledger %d: %v", i, proc, sequence, err)
				return errors.Wrapf(err, "processor %d (%T) failed", i, proc)
			}

			processingTime := time.Since(procStart)
			if processingTime > time.Second {
				log.Printf("Warning: Processor %d (%T) took %v to process ledger %d",
					i, proc, processingTime, sequence)
			} else {
				log.Printf("Processor %d (%T) successfully processed ledger %d in %v",
					i, proc, sequence, processingTime)
			}
		}
	}

	// Move to the next ledger
	src.currentLedger++
	log.Printf("Successfully completed processing ledger %d through %d processors, moving to %d",
		sequence, len(processors), src.currentLedger)

	return nil
}

func (src *RPCLedgerSource) Process(ctx context.Context, msg pluginapi.Message) error {
	// This method is not used for Source plugins, but we'll implement it anyway
	// to satisfy any interface requirements
	return fmt.Errorf("RPCLedgerSource does not support the Process method")
}

func (src *RPCLedgerSource) Close() error {
	// Make sure we stop polling if Close is called
	if src.stopCh != nil {
		close(src.stopCh)
	}
	return nil
}

func New() pluginapi.Plugin {
	return &RPCLedgerSource{
		pollInterval: 1 * time.Second,
		format:       "base64", // Default format
		stopCh:       make(chan struct{}),
	}
}

func main() {
	// This function is required for building as a plugin
	// The actual plugin is loaded through the New() function
}
