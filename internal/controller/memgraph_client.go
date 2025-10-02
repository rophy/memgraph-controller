package controller

import (
	"context"
	"encoding/json"
	"fmt"
	"strings"
	"time"

	"memgraph-controller/internal/common"

	"gopkg.in/yaml.v3"
)

type MemgraphClient struct {
	config         *common.Config
	connectionPool *ConnectionPool // Will be set from ClusterState
}

type ReplicationRole struct {
	Role string
}

type ReplicaInfo struct {
	Name           string          // "memgraph_ha_0"
	SocketAddress  string          // "ip:port"
	SyncMode       string          // "sync" or "async"
	SystemInfo     string          // Raw system_info field from SHOW REPLICAS
	DataInfo       string          // Raw data_info field from SHOW REPLICAS
	ParsedDataInfo *DataInfoStatus // Parsed structure from data_info
}

// DataInfoStatus represents parsed data_info content for replication health
type DataInfoStatus struct {
	Behind      int    `json:"behind"`                 // Replication lag (-1 indicates error)
	Status      string `json:"status"`                 // "ready", "invalid", etc.
	Timestamp   int    `json:"ts"`                     // Sequence timestamp
	IsHealthy   bool   `json:"is_healthy"`             // Computed health status
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

// GetPodName returns the pod name converted from replica name (underscores to dashes)
func (ri *ReplicaInfo) GetPodName() string {
	return strings.ReplaceAll(ri.Name, "_", "-")
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
	case status == "replicating":
		// Replicating is a normal transitional state during data synchronization
		if behind < 0 {
			return false, fmt.Sprintf("Replication in progress but behind is negative: %d", behind)
		}
		// Don't mark as healthy (IsHealthy=false) but provide informative message
		return false, "Replication in progress (transitional state)"
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

func NewMemgraphClient(config *common.Config) *MemgraphClient {
	return &MemgraphClient{
		config:         config,
		connectionPool: NewConnectionPool(config), // Each client has its own connection pool
	}
}

func (mc *MemgraphClient) Close(ctx context.Context) {
	if mc.connectionPool != nil {
		mc.connectionPool.Close(ctx)
	}
}

func (mc *MemgraphClient) QueryReplicationRole(ctx context.Context, boltAddress string) (*ReplicationRole, error) {
	logger := common.GetLoggerFromContext(ctx)

	records, err := mc.connectionPool.RunQueryWithTimeout(ctx, boltAddress, "SHOW REPLICATION ROLE", 5*time.Second)
	if err != nil {
		logger.Error("DEBUG: RunQueryWithTimeout failed", "error", err)
		return nil, fmt.Errorf("failed to query replication role from %s: %w", boltAddress, err)
	}

	if len(records) == 0 {
		// Should never happen.
		logger.Error("☢️QueryReplicationRole: No records returned", "bolt_address", boltAddress)
		return nil, fmt.Errorf("no results returned from SHOW REPLICATION ROLE")
	}

	record := records[0]
	role, found := record.Get("replication role")

	if !found {
		// Should never happen.
		logger.Error("☢️QueryReplicationRole: replication role not found in result", "bolt_address", boltAddress)
		return nil, fmt.Errorf("replication role not found in result")
	}

	roleStr, ok := role.(string)
	if !ok {
		// Should never happen.
		logger.Error("☢️QueryReplicationRole: replication role is not a string", "bolt_address", boltAddress, "role", role)
		return nil, fmt.Errorf("replication role is not a string: %T", role)
	}

	replicationRole := &ReplicationRole{Role: roleStr}
	return replicationRole, nil
}

// QueryReplicas queries the replicas with retry logic and connection pooling
func (mc *MemgraphClient) QueryReplicas(ctx context.Context, boltAddress string) (*ReplicasResponse, error) {
	logger := common.GetLoggerFromContext(ctx)
	records, err := mc.connectionPool.RunQueryWithTimeout(ctx, boltAddress, "SHOW REPLICAS", 5*time.Second)
	if err != nil {
		return nil, fmt.Errorf("failed to query replicas from %s: %w", boltAddress, err)
	}

	var replicas []ReplicaInfo
	for _, record := range records {

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
					logger.Warn("Failed to parse data_info for replica", "replica_name", replica.Name, "error", err)
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
					logger.Warn("Failed to parse data_info map for replica", "replica_name", replica.Name, "error", err)
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

	result := &ReplicasResponse{Replicas: replicas}

	logger.Info("Queried replicas", "bolt_address", boltAddress, "replica_count", len(result.Replicas))
	for _, replica := range result.Replicas {
		logger.Info("Replica found",
			"name", replica.Name,
			"socket_address", replica.SocketAddress,
			"sync_mode", replica.SyncMode,
			"parsed_data_info", replica.ParsedDataInfo,
		)
	}

	return result, nil
}

// Ping verifies target bolt address is reachable
func (mc *MemgraphClient) Ping(ctx context.Context, boltAddress string, timeout time.Duration) error {
	_, err := mc.connectionPool.RunQueryWithTimeout(ctx, boltAddress, "RETURN 1", timeout)
	if err != nil {
		return fmt.Errorf("failed to ping %s: %w", boltAddress, err)
	}
	return nil
}

// SetReplicationRoleToMain sets the replication role to MAIN
func (mc *MemgraphClient) SetReplicationRoleToMain(ctx context.Context, boltAddress string) error {
	_, err := mc.connectionPool.RunQueryWithTimeout(ctx, boltAddress, "SET REPLICATION ROLE TO MAIN", 5*time.Second)
	if err != nil {
		return fmt.Errorf("failed to set replication role to MAIN for %s: %w", boltAddress, err)
	}
	return nil
}

// SetReplicationRoleToReplica sets the replication role to REPLICA
func (mc *MemgraphClient) SetReplicationRoleToReplica(ctx context.Context, boltAddress string) error {
	_, err := mc.connectionPool.RunQueryWithTimeout(ctx, boltAddress, "SET REPLICATION ROLE TO REPLICA WITH PORT 10000", 5*time.Second)
	if err != nil {
		return fmt.Errorf("failed to set replication role to REPLICA for %s: %w", boltAddress, err)
	}
	return nil
}

// RegisterReplica registers a replica with the main using specified mode (STRICT_SYNC or ASYNC)
func (mc *MemgraphClient) RegisterReplica(ctx context.Context, mainBoltAddress, replicaName, replicaAddress, syncMode string) error {
	if mainBoltAddress == "" {
		return fmt.Errorf("main bolt address is empty")
	}
	if replicaName == "" {
		return fmt.Errorf("replica name is empty")
	}
	if replicaAddress == "" {
		return fmt.Errorf("replica address is empty")
	}
	if syncMode != "STRICT_SYNC" && syncMode != "ASYNC" {
		return fmt.Errorf("invalid sync mode: %s (must be STRICT_SYNC or ASYNC)", syncMode)
	}
	_, err := mc.connectionPool.RunQueryWithTimeout(ctx, mainBoltAddress, fmt.Sprintf("REGISTER REPLICA %s %s TO \"%s\"", replicaName, syncMode, replicaAddress), 5*time.Second)
	if err != nil {
		return fmt.Errorf("failed to register replica %s: %w", replicaName, err)
	}
	return nil
}

// DropReplica removes a replica registration from the main
func (mc *MemgraphClient) DropReplica(ctx context.Context, mainBoltAddress, replicaName string) error {
	_, err := mc.connectionPool.RunQueryWithTimeout(ctx, mainBoltAddress, fmt.Sprintf("DROP REPLICA %s", replicaName), 5*time.Second)
	if err != nil {
		return fmt.Errorf("failed to drop replica %s: %w", replicaName, err)
	}
	return nil
}

// QueryStorageInfo queries storage information
func (mc *MemgraphClient) QueryStorageInfo(ctx context.Context, boltAddress string) (*StorageInfo, error) {
	if boltAddress == "" {
		return nil, fmt.Errorf("bolt address is empty")
	}

	var storageInfo *StorageInfo

	// Execute SHOW STORAGE INFO
	records, err := mc.connectionPool.RunQueryWithTimeout(ctx, boltAddress, "SHOW STORAGE INFO", 5*time.Second)
	if err != nil {
		return nil, fmt.Errorf("failed to execute SHOW STORAGE INFO: %w", err)
	}

	storageInfo = &StorageInfo{}
	for _, record := range records {

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

	return storageInfo, nil
}
