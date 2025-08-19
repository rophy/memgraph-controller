package controller

import (
	"context"
	"fmt"
	"log"
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
}

type ReplicasResponse struct {
	Replicas []ReplicaInfo
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
		log.Printf("  Replica: %s at %s, sync=%s", replica.Name, replica.SocketAddress, replica.SyncMode)
	}

	return replicasResponse, nil
}