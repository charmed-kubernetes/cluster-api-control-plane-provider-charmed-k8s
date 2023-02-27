package juju

import (
	"context"
	"fmt"
	"strings"

	"github.com/juju/juju/api"
	"github.com/juju/juju/api/client/keymanager"
	"github.com/juju/juju/rpc/params"
	"github.com/juju/utils/v3/ssh"
)

type keysClient struct {
	ConnectionFactory
}

func newKeysClient(cf ConnectionFactory) *keysClient {
	return &keysClient{
		ConnectionFactory: cf,
	}
}

// GetModelUUID retrieves a model UUID by name
func (c *keysClient) ListKeys(ctx context.Context, modelUUID string) ([]params.StringsResult, error) {
	conn, err := c.GetConnection(ctx, &modelUUID)
	if err != nil {
		return nil, err
	}

	currentUser := c.getCurrentUser(conn)
	client := keymanager.NewClient(conn)
	defer client.Close()

	results, err := client.ListKeys(ssh.ListMode(true), currentUser)
	if err != nil {
		return nil, err
	}

	return results, nil
}

func (c *keysClient) AddKey(ctx context.Context, modelUUID string, key string) (*params.ErrorResult, error) {
	conn, err := c.GetConnection(ctx, &modelUUID)
	if err != nil {
		return nil, err
	}

	currentUser := c.getCurrentUser(conn)
	client := keymanager.NewClient(conn)
	defer client.Close()

	results, err := client.AddKeys(currentUser, key)
	if err != nil {
		return nil, err
	}

	if len(results) != 1 {
		return nil, fmt.Errorf("unexpected length of returned results, expected 1 got %d", len(results))

	}

	return &results[0], nil
}

func (c *keysClient) getCurrentUser(conn api.Connection) string {
	return strings.TrimPrefix(conn.AuthTag().String(), PrefixUser)
}
