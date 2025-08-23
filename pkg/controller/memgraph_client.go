package controller

import (
	"context"
	"fmt"
	"log"
	"regexp"
	"strconv"
	"strings"
	"time"

	"github.com/neo4j/neo4j-go-driver/v5/neo4j"
)

type MemgraphClient struct {
	config         *Config
	connectionPool *ConnectionPool
	retryConfig    RetryConfig
}

type ReplicationRole struct {
	Role string
}

type ReplicaInfo struct {
	Name         string
	SocketAddress string
	SyncMode     string
	SystemTimestamp int64
	CheckFrequency  int64
	DataInfo     string           // Raw data_info field from SHOW REPLICAS
	ParsedDataInfo *DataInfoStatus // Parsed structure
}

// DataInfoStatus represents parsed data_info content for replication health
type DataInfoStatus struct {
	Behind      int    `json:"behind"`      // Replication lag (-1 indicates error)
	Status      string `json:"status"`      // "ready", "invalid", etc.
	Timestamp   int    `json:"ts"`          // Sequence timestamp
	IsHealthy   bool   `json:"is_healthy"`  // Computed health status
	ErrorReason string `json:"error_reason,omitempty"` // Human-readable error description
}

type ReplicasResponse struct {
	Replicas []ReplicaInfo
}

// IsHealthy returns true if the replica is in a healthy state based on data_info
func (ri *ReplicaInfo) IsHealthy() bool {
	if ri.ParsedDataInfo == nil {
		return false
	}
	return ri.ParsedDataInfo.IsHealthy
}

// GetHealthReason returns a human-readable description of the replica health status
func (ri *ReplicaInfo) GetHealthReason() string {
	if ri.ParsedDataInfo == nil {
		return "No health information available"
	}
	if ri.ParsedDataInfo.IsHealthy {
		return "Replica is healthy"
	}
	return ri.ParsedDataInfo.ErrorReason
}

// RequiresRecovery determines if this replica needs recovery intervention
func (ri *ReplicaInfo) RequiresRecovery() bool {
	if ri.ParsedDataInfo == nil {
		return false
	}
	
	switch ri.ParsedDataInfo.Status {
	case "invalid":
		return true // Connection/registration failure - needs re-registration
	case "empty":
		return true // Connection establishment failure - needs investigation  
	case "malformed", "parse_error":
		return false // Parsing issue - not a replication failure
	case "ready":
		return false // Healthy state
	default:
		// For unknown statuses, check if behind indicates problems
		return ri.ParsedDataInfo.Behind < 0
	}
}

// GetRecoveryAction returns the recommended recovery action for unhealthy replicas
func (ri *ReplicaInfo) GetRecoveryAction() string {
	if ri.IsHealthy() {
		return "none"
	}
	
	if ri.ParsedDataInfo == nil {
		return "investigate"
	}
	
	switch ri.ParsedDataInfo.Status {
	case "invalid":
		return "re-register" // Drop and re-register the replica
	case "empty":
		return "restart-pod" // Potential pod restart needed
	case "malformed", "parse_error":
		return "investigate" // Manual investigation needed
	default:
		if ri.ParsedDataInfo.Behind < -10 {
			return "manual-intervention" // Severe lag requires manual check
		}
		return "re-register" // Default recovery attempt
	}
}

// parseDataInfo parses the data_info field from SHOW REPLICAS output
// Expected formats:
// - Healthy ASYNC: "{memgraph: {behind: 0, status: \"ready\", ts: 2}}"  
// - Unhealthy ASYNC: "{memgraph: {behind: -20, status: \"invalid\", ts: 0}}"
// - SYNC replica: "{}" (empty)
// - Missing/malformed: ""
func parseDataInfo(dataInfoStr string) (*DataInfoStatus, error) {
	status := &DataInfoStatus{}
	
	// Handle empty or whitespace-only strings
	dataInfoStr = strings.TrimSpace(dataInfoStr)
	if dataInfoStr == "" {
		status.Status = "unknown"
		status.Behind = -1
		status.IsHealthy = false
		status.ErrorReason = "Missing data_info field"
		return status, nil
	}
	
	// Handle empty JSON object (common for SYNC replicas)
	if dataInfoStr == "{}" {
		status.Status = "empty"
		status.Behind = 0
		status.IsHealthy = false // Empty typically means connection issue
		status.ErrorReason = "Empty data_info - possible connection failure"
		return status, nil
	}
	
	// Parse JSON-like structure: "{memgraph: {behind: 0, status: \"ready\", ts: 2}}"
	// This is not valid JSON, so we need custom parsing
	
	// Extract the inner memgraph object using regex
	re := regexp.MustCompile(`\{memgraph:\s*\{([^}]+)\}\}`)
	matches := re.FindStringSubmatch(dataInfoStr)
	if len(matches) < 2 {
		status.Status = "malformed"
		status.Behind = -1
		status.IsHealthy = false
		status.ErrorReason = fmt.Sprintf("Unable to parse data_info format: %s", dataInfoStr)
		return status, nil
	}
	
	// Parse the inner content: "behind: 0, status: \"ready\", ts: 2"
	innerContent := matches[1]
	
	// Extract behind value
	behindRe := regexp.MustCompile(`behind:\s*(-?\d+)`)
	if behindMatches := behindRe.FindStringSubmatch(innerContent); len(behindMatches) >= 2 {
		if behind, err := strconv.Atoi(behindMatches[1]); err == nil {
			status.Behind = behind
		}
	} else {
		status.Behind = -1 // Default to error state
	}
	
	// Extract status value  
	statusRe := regexp.MustCompile(`status:\s*"([^"]+)"`)
	if statusMatches := statusRe.FindStringSubmatch(innerContent); len(statusMatches) >= 2 {
		status.Status = statusMatches[1]
	} else {
		status.Status = "unknown"
	}
	
	// Extract timestamp value
	tsRe := regexp.MustCompile(`ts:\s*(\d+)`)
	if tsMatches := tsRe.FindStringSubmatch(innerContent); len(tsMatches) >= 2 {
		if ts, err := strconv.Atoi(tsMatches[1]); err == nil {
			status.Timestamp = ts
		}
	}
	
	// Determine health status based on parsed values
	status.IsHealthy, status.ErrorReason = assessReplicationHealth(status.Status, status.Behind)
	
	return status, nil
}

// assessReplicationHealth determines if replica is healthy based on status and lag
func assessReplicationHealth(status string, behind int) (bool, string) {
	switch {
	case status == "ready" && behind >= 0:
		return true, ""
	case status == "invalid":
		return false, "Replication status marked as invalid"
	case behind < 0:
		return false, fmt.Sprintf("Negative replication lag detected: %d", behind)  
	case status == "empty":
		return false, "Empty data_info indicates connection failure"
	case status == "unknown" || status == "malformed":
		return false, "Unable to determine replication health"
	default:
		// Unknown status but non-negative lag - treat as degraded
		return false, fmt.Sprintf("Unknown replication status: %s", status)
	}
}

func NewMemgraphClient(config *Config) *MemgraphClient {
	return &MemgraphClient{
		config:         config,
		connectionPool: NewConnectionPool(config),
		retryConfig: RetryConfig{
			MaxRetries: 3,
			BaseDelay:  1 * time.Second,
			MaxDelay:   10 * time.Second,
		},
	}
}

func (mc *MemgraphClient) Close(ctx context.Context) {
	mc.connectionPool.Close(ctx)
}

func (mc *MemgraphClient) QueryReplicationRole(ctx context.Context, boltAddress string) (*ReplicationRole, error) {
	if boltAddress == "" {
		return nil, fmt.Errorf("bolt address is empty")
	}

	// Create driver for this specific instance
	driver, err := neo4j.NewDriverWithContext(
		fmt.Sprintf("bolt://%s", boltAddress),
		neo4j.NoAuth(),
		func(config *neo4j.Config) {
			config.ConnectionAcquisitionTimeout = 10 * time.Second
			config.SocketConnectTimeout = 5 * time.Second
		},
	)
	if err != nil {
		return nil, fmt.Errorf("failed to create driver for %s: %w", boltAddress, err)
	}
	defer driver.Close(ctx)

	// Test connectivity
	err = driver.VerifyConnectivity(ctx)
	if err != nil {
		return nil, fmt.Errorf("failed to verify connectivity to %s: %w", boltAddress, err)
	}

	// Execute SHOW REPLICATION ROLE query
	session := driver.NewSession(ctx, neo4j.SessionConfig{})
	defer session.Close(ctx)

	result, err := session.ExecuteRead(ctx, func(tx neo4j.ManagedTransaction) (interface{}, error) {
		result, err := tx.Run(ctx, "SHOW REPLICATION ROLE", nil)
		if err != nil {
			return nil, err
		}

		if result.Next(ctx) {
			record := result.Record()
			role, found := record.Get("replication_role")
			if !found {
				return nil, fmt.Errorf("replication_role field not found in result")
			}
			
			roleStr, ok := role.(string)
			if !ok {
				return nil, fmt.Errorf("replication_role is not a string: %T", role)
			}

			return &ReplicationRole{Role: roleStr}, nil
		}

		return nil, fmt.Errorf("no results returned from SHOW REPLICATION ROLE")
	})

	if err != nil {
		return nil, fmt.Errorf("failed to query replication role from %s: %w", boltAddress, err)
	}

	replicationRole, ok := result.(*ReplicationRole)
	if !ok {
		return nil, fmt.Errorf("unexpected result type: %T", result)
	}

	log.Printf("Queried replication role from %s: %s", boltAddress, replicationRole.Role)
	return replicationRole, nil
}

func (mc *MemgraphClient) TestConnection(ctx context.Context, boltAddress string) error {
	if boltAddress == "" {
		return fmt.Errorf("bolt address is empty")
	}

	driver, err := neo4j.NewDriverWithContext(
		fmt.Sprintf("bolt://%s", boltAddress),
		neo4j.NoAuth(),
		func(config *neo4j.Config) {
			config.ConnectionAcquisitionTimeout = 5 * time.Second
			config.SocketConnectTimeout = 3 * time.Second
		},
	)
	if err != nil {
		return fmt.Errorf("failed to create driver for %s: %w", boltAddress, err)
	}
	defer driver.Close(ctx)

	err = driver.VerifyConnectivity(ctx)
	if err != nil {
		return fmt.Errorf("failed to verify connectivity to %s: %w", boltAddress, err)
	}

	log.Printf("Successfully connected to Memgraph at %s", boltAddress)
	return nil
}

func (mc *MemgraphClient) QueryReplicas(ctx context.Context, boltAddress string) (*ReplicasResponse, error) {
	if boltAddress == "" {
		return nil, fmt.Errorf("bolt address is empty")
	}

	// Create driver for this specific instance
	driver, err := neo4j.NewDriverWithContext(
		fmt.Sprintf("bolt://%s", boltAddress),
		neo4j.NoAuth(),
		func(config *neo4j.Config) {
			config.ConnectionAcquisitionTimeout = 10 * time.Second
			config.SocketConnectTimeout = 5 * time.Second
		},
	)
	if err != nil {
		return nil, fmt.Errorf("failed to create driver for %s: %w", boltAddress, err)
	}
	defer driver.Close(ctx)

	// Test connectivity
	err = driver.VerifyConnectivity(ctx)
	if err != nil {
		return nil, fmt.Errorf("failed to verify connectivity to %s: %w", boltAddress, err)
	}

	// Execute SHOW REPLICAS query
	session := driver.NewSession(ctx, neo4j.SessionConfig{})
	defer session.Close(ctx)

	result, err := session.ExecuteRead(ctx, func(tx neo4j.ManagedTransaction) (interface{}, error) {
		result, err := tx.Run(ctx, "SHOW REPLICAS", nil)
		if err != nil {
			return nil, err
		}

		var replicas []ReplicaInfo
		for result.Next(ctx) {
			record := result.Record()
			
			replica := ReplicaInfo{}
			
			if name, found := record.Get("name"); found {
				if nameStr, ok := name.(string); ok {
					replica.Name = nameStr
				}
			}
			
			if socketAddr, found := record.Get("socket_address"); found {
				if socketAddrStr, ok := socketAddr.(string); ok {
					replica.SocketAddress = socketAddrStr
				}
			}
			
			if syncMode, found := record.Get("sync_mode"); found {
				if syncModeStr, ok := syncMode.(string); ok {
					replica.SyncMode = syncModeStr
				}
			}
			
			if sysTimestamp, found := record.Get("system_timestamp"); found {
				if sysTimestampInt, ok := sysTimestamp.(int64); ok {
					replica.SystemTimestamp = sysTimestampInt
				}
			}
			
			if checkFreq, found := record.Get("check_frequency"); found {
				if checkFreqInt, ok := checkFreq.(int64); ok {
					replica.CheckFrequency = checkFreqInt
				}
			}
			
			// Extract and parse data_info field
			if dataInfo, found := record.Get("data_info"); found {
				if dataInfoStr, ok := dataInfo.(string); ok {
					replica.DataInfo = dataInfoStr
					
					// Parse the data_info field for health assessment
					if parsed, err := parseDataInfo(dataInfoStr); err != nil {
						log.Printf("Warning: Failed to parse data_info for replica %s: %v", replica.Name, err)
						// Create fallback status
						replica.ParsedDataInfo = &DataInfoStatus{
							Status:      "parse_error",
							Behind:      -1,
							IsHealthy:   false,
							ErrorReason: fmt.Sprintf("Parse error: %v", err),
						}
					} else {
						replica.ParsedDataInfo = parsed
					}
				}
			} else {
				// No data_info field found - create default status
				replica.DataInfo = ""
				replica.ParsedDataInfo = &DataInfoStatus{
					Status:      "missing",
					Behind:      -1,
					IsHealthy:   false,
					ErrorReason: "data_info field not present in SHOW REPLICAS output",
				}
			}
			
			replicas = append(replicas, replica)
		}

		return &ReplicasResponse{Replicas: replicas}, nil
	})

	if err != nil {
		return nil, fmt.Errorf("failed to query replicas from %s: %w", boltAddress, err)
	}

	replicasResponse, ok := result.(*ReplicasResponse)
	if !ok {
		return nil, fmt.Errorf("unexpected result type: %T", result)
	}

	log.Printf("Queried replicas from %s: found %d replicas", boltAddress, len(replicasResponse.Replicas))
	for _, replica := range replicasResponse.Replicas {
		healthStatus := "unknown"
		healthReason := ""
		if replica.ParsedDataInfo != nil {
			if replica.ParsedDataInfo.IsHealthy {
				healthStatus = "healthy"
			} else {
				healthStatus = "unhealthy"
				healthReason = fmt.Sprintf(" (%s)", replica.ParsedDataInfo.ErrorReason)
			}
		}
		log.Printf("  Replica: %s at %s, sync=%s, health=%s%s", 
			replica.Name, replica.SocketAddress, replica.SyncMode, healthStatus, healthReason)
	}

	return replicasResponse, nil
}