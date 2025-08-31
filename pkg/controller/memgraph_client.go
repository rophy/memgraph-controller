package controller

import (
	"context"
	"encoding/json"
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
	connectionPool *ConnectionPool // Will be set from ClusterState
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

// StorageInfo represents the response from SHOW STORAGE INFO
type StorageInfo struct {
	VertexCount int64
	EdgeCount   int64
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
		case int64:
			status.Behind = int(v)
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
		case int64:
			status.Timestamp = int(v)
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
		connectionPool: NewConnectionPool(config), // Create own connection pool initially
		retryConfig: RetryConfig{
			MaxRetries: 3,
			BaseDelay:  1 * time.Second,
			MaxDelay:   10 * time.Second,
		},
	}
}

// SetConnectionPool sets the connection pool from ClusterState
func (mc *MemgraphClient) SetConnectionPool(connectionPool *ConnectionPool) {
	mc.connectionPool = connectionPool
}

func (mc *MemgraphClient) Close(ctx context.Context) {
	if mc.connectionPool != nil {
		mc.connectionPool.Close(ctx)
	}
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

// QueryReplicationRoleWithRetry queries the replication role with retry logic and connection pooling
func (mc *MemgraphClient) QueryReplicationRoleWithRetry(ctx context.Context, boltAddress string) (*ReplicationRole, error) {
	if boltAddress == "" {
		return nil, fmt.Errorf("bolt address is empty")
	}

	var result *ReplicationRole
	err := WithRetry(ctx, func() error {
		driver, err := mc.connectionPool.GetDriver(ctx, boltAddress)
		if err != nil {
			return fmt.Errorf("failed to get driver for %s: %w", boltAddress, err)
		}

		session := driver.NewSession(ctx, neo4j.SessionConfig{})
		defer func() {
			if closeErr := session.Close(ctx); closeErr != nil {
				log.Printf("Warning: failed to close session for %s: %v", boltAddress, closeErr)
			}
		}()

		// Use auto-commit mode for replication queries
		txResult, err := session.Run(ctx, "SHOW REPLICATION ROLE", nil)
		if err != nil {
			return fmt.Errorf("failed to execute SHOW REPLICATION ROLE: %w", err)
		}

		if txResult.Next(ctx) {
			record := txResult.Record()
			role, found := record.Get("replication role")
			if !found {
				return fmt.Errorf("replication role field not found in result")
			}

			roleStr, ok := role.(string)
			if !ok {
				return fmt.Errorf("replication role is not a string: %T", role)
			}

			result = &ReplicationRole{Role: roleStr}
			return nil
		}

		return fmt.Errorf("no results returned from SHOW REPLICATION ROLE")
	}, mc.retryConfig)

	if err != nil {
		return nil, fmt.Errorf("failed to query replication role from %s after retries: %w", boltAddress, err)
	}

	log.Printf("Queried replication role from %s: %s", boltAddress, result.Role)
	return result, nil
}

// QueryReplicasWithRetry queries the replicas with retry logic and connection pooling
func (mc *MemgraphClient) QueryReplicasWithRetry(ctx context.Context, boltAddress string) (*ReplicasResponse, error) {
	if boltAddress == "" {
		return nil, fmt.Errorf("bolt address is empty")
	}

	var result *ReplicasResponse
	err := WithRetry(ctx, func() error {
		driver, err := mc.connectionPool.GetDriver(ctx, boltAddress)
		if err != nil {
			return fmt.Errorf("failed to get driver for %s: %w", boltAddress, err)
		}

		session := driver.NewSession(ctx, neo4j.SessionConfig{})
		defer func() {
			if closeErr := session.Close(ctx); closeErr != nil {
				log.Printf("Warning: failed to close session for %s: %v", boltAddress, closeErr)
			}
		}()

		// Use auto-commit mode for replication queries
		txResult, err := session.Run(ctx, "SHOW REPLICAS", nil)
		if err != nil {
			return fmt.Errorf("failed to execute SHOW REPLICAS: %w", err)
		}

		var replicas []ReplicaInfo
		for txResult.Next(ctx) {
			record := txResult.Record()

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

			// Extract system_info field
			if systemInfo, found := record.Get("system_info"); found {
				if systemInfoStr, ok := systemInfo.(string); ok {
					replica.SystemInfo = systemInfoStr
				}
			}
			
			// Extract and parse data_info field
			if dataInfo, found := record.Get("data_info"); found {
				if dataInfoStr, ok := dataInfo.(string); ok {
					// Case 1: data_info is a string (as expected)
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
				} else if dataInfoMap, ok := dataInfo.(map[string]interface{}); ok {
					// Case 2: data_info is a map (Neo4j driver auto-parsed the YAML)
					
					// Convert map back to JSON string for storage
					if jsonBytes, err := json.Marshal(dataInfoMap); err == nil {
						replica.DataInfo = string(jsonBytes)
					} else {
						replica.DataInfo = fmt.Sprintf("%+v", dataInfoMap)
					}
					
					// Parse the map directly for health assessment
					if parsed, err := parseDataInfoMap(dataInfoMap); err != nil {
						log.Printf("Warning: Failed to parse data_info map for replica %s: %v", replica.Name, err)
						replica.ParsedDataInfo = &DataInfoStatus{
							Status:      "parse_error",
							Behind:      -1,
							IsHealthy:   false,
							ErrorReason: fmt.Sprintf("Map parse error: %v", err),
						}
					} else {
						replica.ParsedDataInfo = parsed
					}
				} else {
					replica.DataInfo = fmt.Sprintf("%v", dataInfo)
					replica.ParsedDataInfo = &DataInfoStatus{
						Status:      "type_error",
						Behind:      -1,
						IsHealthy:   false,
						ErrorReason: fmt.Sprintf("data_info field has unexpected type: %T", dataInfo),
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

		result = &ReplicasResponse{Replicas: replicas}
		return nil
	}, mc.retryConfig)

	if err != nil {
		return nil, fmt.Errorf("failed to query replicas from %s after retries: %w", boltAddress, err)
	}

	log.Printf("Queried replicas from %s: found %d replicas", boltAddress, len(result.Replicas))
	for _, replica := range result.Replicas {
		log.Printf("  Replica: %s at %s, sync=%s", replica.Name, replica.SocketAddress, replica.SyncMode)
	}

	return result, nil
}

// TestConnectionWithRetry tests connection with retry logic and connection pooling
func (mc *MemgraphClient) TestConnectionWithRetry(ctx context.Context, boltAddress string) error {
	if boltAddress == "" {
		return fmt.Errorf("bolt address is empty")
	}

	err := WithRetry(ctx, func() error {
		_, err := mc.connectionPool.GetDriver(ctx, boltAddress)
		if err != nil {
			return err
		}

		// Driver verification is already done in GetDriver, so we just check if we got a driver
		return nil
	}, mc.retryConfig)

	if err != nil {
		return fmt.Errorf("failed to connect to %s after retries: %w", boltAddress, err)
	}

	log.Printf("Successfully connected to Memgraph at %s", boltAddress)
	return nil
}

// SetReplicationRoleToMainWithRetry sets the replication role to MAIN with retry logic
func (mc *MemgraphClient) SetReplicationRoleToMainWithRetry(ctx context.Context, boltAddress string) error {
	if boltAddress == "" {
		return fmt.Errorf("bolt address is empty")
	}

	err := WithRetry(ctx, func() error {
		driver, err := mc.connectionPool.GetDriver(ctx, boltAddress)
		if err != nil {
			return fmt.Errorf("failed to get driver for %s: %w", boltAddress, err)
		}

		session := driver.NewSession(ctx, neo4j.SessionConfig{})
		defer func() {
			if closeErr := session.Close(ctx); closeErr != nil {
				log.Printf("Warning: failed to close session for %s: %v", boltAddress, closeErr)
			}
		}()

		// Use auto-commit mode for replication commands
		_, err = session.Run(ctx, "SET REPLICATION ROLE TO MAIN", nil)
		if err != nil {
			return fmt.Errorf("failed to execute SET REPLICATION ROLE TO MAIN: %w", err)
		}

		log.Printf("Successfully set replication role to MAIN for %s", boltAddress)
		return nil
	}, mc.retryConfig)

	if err != nil {
		return fmt.Errorf("failed to set replication role to MAIN for %s after retries: %w", boltAddress, err)
	}

	return nil
}

// SetReplicationRoleToMain sets the replication role to MAIN without retry (fast failover)
func (mc *MemgraphClient) SetReplicationRoleToMain(ctx context.Context, boltAddress string) error {
	if boltAddress == "" {
		return fmt.Errorf("bolt address is empty")
	}

	driver, err := mc.connectionPool.GetDriver(ctx, boltAddress)
	if err != nil {
		return fmt.Errorf("failed to get driver for %s: %w", boltAddress, err)
	}

	session := driver.NewSession(ctx, neo4j.SessionConfig{})
	defer func() {
		if closeErr := session.Close(ctx); closeErr != nil {
			log.Printf("Warning: failed to close session for %s: %v", boltAddress, closeErr)
		}
	}()

	// Use auto-commit mode for replication commands
	_, err = session.Run(ctx, "SET REPLICATION ROLE TO MAIN", nil)
	if err != nil {
		return fmt.Errorf("failed to execute SET REPLICATION ROLE TO MAIN: %w", err)
	}

	log.Printf("Successfully set replication role to MAIN for %s (fast failover)", boltAddress)
	return nil
}

// SetReplicationRoleToReplicaWithRetry sets the replication role to REPLICA with retry logic
func (mc *MemgraphClient) SetReplicationRoleToReplicaWithRetry(ctx context.Context, boltAddress string) error {
	if boltAddress == "" {
		return fmt.Errorf("bolt address is empty")
	}

	err := WithRetry(ctx, func() error {
		driver, err := mc.connectionPool.GetDriver(ctx, boltAddress)
		if err != nil {
			return fmt.Errorf("failed to get driver for %s: %w", boltAddress, err)
		}

		session := driver.NewSession(ctx, neo4j.SessionConfig{})
		defer func() {
			if closeErr := session.Close(ctx); closeErr != nil {
				log.Printf("Warning: failed to close session for %s: %v", boltAddress, closeErr)
			}
		}()

		// Use auto-commit mode for replication commands
		_, err = session.Run(ctx, "SET REPLICATION ROLE TO REPLICA WITH PORT 10000", nil)
		if err != nil {
			return fmt.Errorf("failed to execute SET REPLICATION ROLE TO REPLICA: %w", err)
		}

		log.Printf("Successfully set replication role to REPLICA for %s", boltAddress)
		return nil
	}, mc.retryConfig)

	if err != nil {
		return fmt.Errorf("failed to set replication role to REPLICA for %s after retries: %w", boltAddress, err)
	}

	return nil
}

// RegisterReplicaWithModeAndRetry registers a replica with the main using specified mode (SYNC or ASYNC)
func (mc *MemgraphClient) RegisterReplicaWithModeAndRetry(ctx context.Context, mainBoltAddress, replicaName, replicaAddress, syncMode string) error {
	if mainBoltAddress == "" {
		return fmt.Errorf("main bolt address is empty")
	}
	if replicaName == "" {
		return fmt.Errorf("replica name is empty")
	}
	if replicaAddress == "" {
		return fmt.Errorf("replica address is empty")
	}
	if syncMode != "SYNC" && syncMode != "ASYNC" {
		return fmt.Errorf("invalid sync mode: %s (must be SYNC or ASYNC)", syncMode)
	}

	err := WithRetry(ctx, func() error {
		driver, err := mc.connectionPool.GetDriver(ctx, mainBoltAddress)
		if err != nil {
			return fmt.Errorf("failed to get driver for %s: %w", mainBoltAddress, err)
		}

		session := driver.NewSession(ctx, neo4j.SessionConfig{})
		defer func() {
			if closeErr := session.Close(ctx); closeErr != nil {
				log.Printf("Warning: failed to close session for %s: %v", mainBoltAddress, closeErr)
			}
		}()

		// Use auto-commit mode for replication commands
		query := fmt.Sprintf("REGISTER REPLICA %s %s TO \"%s\"", replicaName, syncMode, replicaAddress)
		_, err = session.Run(ctx, query, nil)
		if err != nil {
			return fmt.Errorf("failed to execute REGISTER REPLICA: %w", err)
		}

		return nil
	}, mc.retryConfig)

	if err != nil {
		return fmt.Errorf("register replica %s (%s mode) with main at %s: %w", replicaName, syncMode, mainBoltAddress, err)
	}

	return nil
}

// RegisterReplicaWithRetry registers a replica with the main using ASYNC mode (backward compatibility)
func (mc *MemgraphClient) RegisterReplicaWithRetry(ctx context.Context, mainBoltAddress, replicaName, replicaAddress string) error {
	return mc.RegisterReplicaWithModeAndRetry(ctx, mainBoltAddress, replicaName, replicaAddress, "ASYNC")
}

// DropReplicaWithRetry removes a replica registration from the main
func (mc *MemgraphClient) DropReplicaWithRetry(ctx context.Context, mainBoltAddress, replicaName string) error {
	if mainBoltAddress == "" {
		return fmt.Errorf("main bolt address is empty")
	}
	if replicaName == "" {
		return fmt.Errorf("replica name is empty")
	}

	err := WithRetry(ctx, func() error {
		driver, err := mc.connectionPool.GetDriver(ctx, mainBoltAddress)
		if err != nil {
			return fmt.Errorf("failed to get driver for %s: %w", mainBoltAddress, err)
		}

		session := driver.NewSession(ctx, neo4j.SessionConfig{})
		defer func() {
			if closeErr := session.Close(ctx); closeErr != nil {
				log.Printf("Warning: failed to close session for %s: %v", mainBoltAddress, closeErr)
			}
		}()

		// Use auto-commit mode for replication commands
		query := fmt.Sprintf("DROP REPLICA %s", replicaName)
		_, err = session.Run(ctx, query, nil)
		if err != nil {
			return fmt.Errorf("failed to execute DROP REPLICA: %w", err)
		}

		log.Printf("Successfully dropped replica %s from main %s", replicaName, mainBoltAddress)
		return nil
	}, mc.retryConfig)

	if err != nil {
		return fmt.Errorf("failed to drop replica %s from main %s after retries: %w",
			replicaName, mainBoltAddress, err)
	}

	return nil
}


// QueryStorageInfoWithRetry queries storage information with retry logic
func (mc *MemgraphClient) QueryStorageInfoWithRetry(ctx context.Context, boltAddress string) (*StorageInfo, error) {
	if boltAddress == "" {
		return nil, fmt.Errorf("bolt address is empty")
	}

	var result *StorageInfo
	err := WithRetry(ctx, func() error {
		driver, err := mc.connectionPool.GetDriver(ctx, boltAddress)
		if err != nil {
			return fmt.Errorf("failed to get driver for %s: %w", boltAddress, err)
		}

		session := driver.NewSession(ctx, neo4j.SessionConfig{})
		defer func() {
			if closeErr := session.Close(ctx); closeErr != nil {
				log.Printf("Warning: failed to close session for %s: %v", boltAddress, closeErr)
			}
		}()

		// Execute SHOW STORAGE INFO
		txResult, err := session.Run(ctx, "SHOW STORAGE INFO", nil)
		if err != nil {
			return fmt.Errorf("failed to execute SHOW STORAGE INFO: %w", err)
		}

		storageInfo := &StorageInfo{}
		for txResult.Next(ctx) {
			record := txResult.Record()
			
			// Extract storage_info field which is typically a string representation
			if storageInfoField, found := record.Get("storage info"); found {
				if _, ok := storageInfoField.(string); ok {
					// Parse the storage info string to extract vertex and edge counts
					// This is a simplified parser - in practice might need more robust parsing
					storageInfo.VertexCount = 0 // Default to 0
					storageInfo.EdgeCount = 0   // Default to 0
					
					// For now, assume empty storage (suitable for bootstrap check)
					// TODO: Implement proper storage info parsing if needed
				}
			}
			
			// Alternative: look for specific vertex_count and edge_count fields
			if vertexCount, found := record.Get("vertex_count"); found {
				if count, ok := vertexCount.(int64); ok {
					storageInfo.VertexCount = count
				}
			}
			if edgeCount, found := record.Get("edge_count"); found {
				if count, ok := edgeCount.(int64); ok {
					storageInfo.EdgeCount = count
				}
			}
		}

		result = storageInfo
		return nil
	}, mc.retryConfig)

	if err != nil {
		return nil, fmt.Errorf("failed to query storage info from %s after retries: %w", boltAddress, err)
	}

	return result, nil
}

// ExecuteCommandWithRetry executes a raw Memgraph command with retry logic
func (mc *MemgraphClient) ExecuteCommandWithRetry(ctx context.Context, boltAddress, command string) error {
	if boltAddress == "" {
		return fmt.Errorf("bolt address is empty")
	}
	if command == "" {
		return fmt.Errorf("command is empty")
	}

	err := WithRetry(ctx, func() error {
		driver, err := mc.connectionPool.GetDriver(ctx, boltAddress)
		if err != nil {
			return fmt.Errorf("failed to get driver for %s: %w", boltAddress, err)
		}

		session := driver.NewSession(ctx, neo4j.SessionConfig{})
		defer func() {
			if closeErr := session.Close(ctx); closeErr != nil {
				log.Printf("Warning: failed to close session for %s: %v", boltAddress, closeErr)
			}
		}()

		// Execute the raw command
		_, err = session.Run(ctx, command, nil)
		if err != nil {
			return fmt.Errorf("failed to execute command '%s': %w", command, err)
		}

		return nil
	}, mc.retryConfig)

	if err != nil {
		return fmt.Errorf("failed to execute command '%s' on %s after retries: %w", command, boltAddress, err)
	}

	log.Printf("Successfully executed command '%s' on %s", command, boltAddress)
	return nil
}

