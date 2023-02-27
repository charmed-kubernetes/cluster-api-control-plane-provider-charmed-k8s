package juju

import (
	"context"
	"fmt"
	"strings"

	"github.com/juju/juju/api"
	"github.com/juju/juju/api/client/modelmanager"
)

type modelsClient struct {
	ConnectionFactory
}

func newModelsClient(cf ConnectionFactory) *modelsClient {
	return &modelsClient{
		ConnectionFactory: cf,
	}
}

func (c *modelsClient) getCurrentUser(conn api.Connection) string {
	return strings.TrimPrefix(conn.AuthTag().String(), PrefixUser)
}

func (c *modelsClient) resolveModelUUIDWithClient(client modelmanager.Client, name string, user string) (string, error) {
	modelUUID := ""
	modelSummaries, err := client.ListModelSummaries(user, false)
	if err != nil {
		return "", err
	}
	for _, modelSummary := range modelSummaries {
		if modelSummary.Name == name {
			modelUUID = modelSummary.UUID
			break
		}
	}

	if modelUUID == "" {
		return "", fmt.Errorf("model not found for defined user")
	}

	return modelUUID, nil
}

// GetModelUUID retrieves a model UUID by name
func (c *modelsClient) GetModelUUID(ctx context.Context, name string) (string, error) {
	conn, err := c.GetConnection(ctx, nil)
	if err != nil {
		return "", err
	}

	currentUser := c.getCurrentUser(conn)
	client := modelmanager.NewClient(conn)
	defer client.Close()

	modelUUID, err := c.resolveModelUUIDWithClient(*client, name, currentUser)
	if err != nil {
		return "", nil
	}

	return modelUUID, nil
}
