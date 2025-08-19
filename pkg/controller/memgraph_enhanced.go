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

// RegisterReplicaWithRetry registers a replica with the master using ASYNC mode
func (mc *MemgraphClient) RegisterReplicaWithRetry(ctx context.Context, masterBoltAddress, replicaName, replicaAddress string) error {
	if masterBoltAddress == "" {
		return fmt.Errorf("master bolt address is empty")
	}
	if replicaName == "" {
		return fmt.Errorf("replica name is empty")
	}
	if replicaAddress == "" {
		return fmt.Errorf("replica address is empty")
	}

	err := WithRetry(ctx, func() error {
		driver, err := mc.connectionPool.GetDriver(ctx, masterBoltAddress)
		if err != nil {
			return fmt.Errorf("failed to get driver for %s: %w", masterBoltAddress, err)
		}

		session := driver.NewSession(ctx, neo4j.SessionConfig{})
		defer func() {
			if closeErr := session.Close(ctx); closeErr != nil {
				log.Printf("Warning: failed to close session for %s: %v", masterBoltAddress, closeErr)
			}
		}()

		// Use auto-commit mode for replication commands
		query := fmt.Sprintf("REGISTER REPLICA %s ASYNC TO \"%s\"", replicaName, replicaAddress)
		_, err = session.Run(ctx, query, nil)
		if err != nil {
			return fmt.Errorf("failed to execute REGISTER REPLICA: %w", err)
		}

		log.Printf("Successfully registered replica %s at %s with master %s (ASYNC mode)", 
			replicaName, replicaAddress, masterBoltAddress)
		return nil
	}, mc.retryConfig)

	if err != nil {
		return fmt.Errorf("failed to register replica %s with master %s after retries: %w", 
			replicaName, masterBoltAddress, err)
	}

	return nil
}

// DropReplicaWithRetry removes a replica registration from the master
func (mc *MemgraphClient) DropReplicaWithRetry(ctx context.Context, masterBoltAddress, replicaName string) error {
	if masterBoltAddress == "" {
		return fmt.Errorf("master bolt address is empty")
	}
	if replicaName == "" {
		return fmt.Errorf("replica name is empty")
	}

	err := WithRetry(ctx, func() error {
		driver, err := mc.connectionPool.GetDriver(ctx, masterBoltAddress)
		if err != nil {
			return fmt.Errorf("failed to get driver for %s: %w", masterBoltAddress, err)
		}

		session := driver.NewSession(ctx, neo4j.SessionConfig{})
		defer func() {
			if closeErr := session.Close(ctx); closeErr != nil {
				log.Printf("Warning: failed to close session for %s: %v", masterBoltAddress, closeErr)
			}
		}()

		// Use auto-commit mode for replication commands
		query := fmt.Sprintf("DROP REPLICA %s", replicaName)
		_, err = session.Run(ctx, query, nil)
		if err != nil {
			return fmt.Errorf("failed to execute DROP REPLICA: %w", err)
		}

		log.Printf("Successfully dropped replica %s from master %s", replicaName, masterBoltAddress)
		return nil
	}, mc.retryConfig)

	if err != nil {
		return fmt.Errorf("failed to drop replica %s from master %s after retries: %w", 
			replicaName, masterBoltAddress, err)
	}

	return nil
}