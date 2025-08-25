package controller

import (
	"context"
	"fmt"
	"log"

	"github.com/neo4j/neo4j-go-driver/v5/neo4j"
)

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

// StorageInfo represents the response from SHOW STORAGE INFO
type StorageInfo struct {
	VertexCount int64
	EdgeCount   int64
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
