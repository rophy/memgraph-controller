package controller

import (
	"context"
	"fmt"
	"log"
	"time"

	"github.com/neo4j/neo4j-go-driver/v5/neo4j"
)

type MemgraphClient struct {
	config *Config
}

type ReplicationRole struct {
	Role string
}

func NewMemgraphClient(config *Config) *MemgraphClient {
	return &MemgraphClient{
		config: config,
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