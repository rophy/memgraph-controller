package controller

import (
	"context"
	"fmt"
	"log"
	"strings"
	"time"

	"github.com/neo4j/neo4j-go-driver/v5/neo4j"
	"github.com/neo4j/neo4j-go-driver/v5/neo4j/config"
	"gopkg.in/yaml.v3"
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
	Name           string
	SocketAddress  string
	SyncMode       string
	SystemInfo     string           // Raw system_info field from SHOW REPLICAS  
	DataInfo       string           // Raw data_info field from SHOW REPLICAS
	ParsedDataInfo *DataInfoStatus  // Parsed structure from data_info
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

// parseDataInfo parses the data_info field from SHOW REPLICAS output using YAML
// Expected formats (valid YAML flow syntax):
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
	
	// Handle empty YAML object (common for SYNC replicas)
	if dataInfoStr == "{}" {
		status.Status = "empty"
		status.Behind = 0
		status.IsHealthy = false // Empty typically means connection issue
		status.ErrorReason = "Empty data_info - possible connection failure"
		return status, nil
	}
	
	// Parse YAML flow syntax: {memgraph: {behind: 0, status: "ready", ts: 2}}
	var yamlData map[string]interface{}
	if err := yaml.Unmarshal([]byte(dataInfoStr), &yamlData); err != nil {
		status.Status = "malformed"
		status.Behind = -1
		status.IsHealthy = false
		status.ErrorReason = fmt.Sprintf("Unable to parse data_info YAML: %v", err)
		return status, nil
	}
	
	// Extract memgraph object
	memgraphData, exists := yamlData["memgraph"]
	if !exists {
		status.Status = "malformed"
		status.Behind = -1
		status.IsHealthy = false
		status.ErrorReason = "Missing 'memgraph' key in data_info"
		return status, nil
	}
	
	// Convert to map for field access
	memgraphMap, ok := memgraphData.(map[string]interface{})
	if !ok {
		status.Status = "malformed"
		status.Behind = -1
		status.IsHealthy = false
		status.ErrorReason = "Invalid memgraph data structure"
		return status, nil
	}
	
	// Extract behind value
	if behindVal, exists := memgraphMap["behind"]; exists {
		if behind, ok := behindVal.(int); ok {
			status.Behind = behind
		} else {
			status.Behind = -1 // Default to error state
		}
	} else {
		status.Behind = -1
	}
	
	// Extract status value
	if statusVal, exists := memgraphMap["status"]; exists {
		if statusStr, ok := statusVal.(string); ok {
			status.Status = statusStr
		} else {
			status.Status = "unknown"
		}
	} else {
		status.Status = "unknown"
	}
	
	// Extract timestamp value
	if tsVal, exists := memgraphMap["ts"]; exists {
		if ts, ok := tsVal.(int); ok {
			status.Timestamp = ts
		}
		// If timestamp is missing or invalid, leave as 0 (default)
	}
	
	// Determine health status based on parsed values
	status.IsHealthy, status.ErrorReason = assessReplicationHealth(status.Status, status.Behind)
	
	return status, nil
}

// parseDataInfoMap parses data_info when it's already a map[string]interface{} 
// (Neo4j driver auto-parsed the YAML string)
func parseDataInfoMap(dataInfoMap map[string]interface{}) (*DataInfoStatus, error) {
	status := &DataInfoStatus{}
	
	// Navigate to memgraph sub-object: {memgraph: {behind: 0, status: "ready", ts: 13}}
	memgraphVal, exists := dataInfoMap["memgraph"]
	if !exists {
		status.Status = "malformed"
		status.Behind = -1
		status.IsHealthy = false
		status.ErrorReason = "Missing 'memgraph' key in data_info"
		return status, nil
	}
	
	memgraphMap, ok := memgraphVal.(map[string]interface{})
	if !ok {
		status.Status = "malformed"
		status.Behind = -1
		status.IsHealthy = false
		status.ErrorReason = "Invalid 'memgraph' value type in data_info"
		return status, nil
	}
	
	// Extract behind value
	if behindVal, exists := memgraphMap["behind"]; exists {
		switch v := behindVal.(type) {
		case int:
			status.Behind = v
		case float64:
			status.Behind = int(v)
		default:
			status.Behind = -1
			status.Status = "malformed"
			status.IsHealthy = false
			status.ErrorReason = fmt.Sprintf("Invalid 'behind' value type: %T", behindVal)
			return status, nil
		}
	} else {
		status.Behind = -1
	}
	
	// Extract status value  
	if statusVal, exists := memgraphMap["status"]; exists {
		if statusStr, ok := statusVal.(string); ok {
			status.Status = statusStr
		} else {
			status.Status = "malformed"
			status.Behind = -1
			status.IsHealthy = false
			status.ErrorReason = fmt.Sprintf("Invalid 'status' value type: %T", statusVal)
			return status, nil
		}
	} else {
		status.Status = "unknown"
	}
	
	// Extract timestamp value
	if tsVal, exists := memgraphMap["ts"]; exists {
		switch v := tsVal.(type) {
		case int:
			status.Timestamp = v
		case float64:
			status.Timestamp = int(v)
		default:
			// Leave as 0 if invalid type
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
		func(c *config.Config) {
			c.ConnectionAcquisitionTimeout = 10 * time.Second
			c.SocketConnectTimeout = 5 * time.Second
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
		func(c *config.Config) {
			c.ConnectionAcquisitionTimeout = 5 * time.Second
			c.SocketConnectTimeout = 3 * time.Second
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

