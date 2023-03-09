package juju

import (
	"context"
	"fmt"
	"strconv"

	"github.com/juju/charm/v9"
	apiapplication "github.com/juju/juju/api/client/application"
	apiclient "github.com/juju/juju/api/client/client"
	"github.com/juju/juju/core/constraints"
	"github.com/juju/juju/rpc/params"
	"github.com/juju/names/v4"
	"sigs.k8s.io/controller-runtime/pkg/log"
)

type applicationsClient struct {
	ConnectionFactory
}

type ReadApplicationInput struct {
	ModelUUID       string
	ApplicationName string
}

type ReadApplicationResponse struct {
	Name        string
	Channel     string
	Revision    int
	Base        params.Base
	Units       int
	Trust       bool
	Config      map[string]ConfigEntry
	Constraints constraints.Value
	Expose      map[string]interface{}
	Principal   bool
	Status      params.ApplicationStatus
}

type ApplicationExistsInput struct {
	ApplicationName string
	ModelUUID       string
}

// ConfigEntry is an auxiliar struct to
// keep information about juju config entries.
// Specially, we want to know if they have the
// default value.
type ConfigEntry struct {
	Value     interface{}
	IsDefault bool
}

// ConfigEntryToString returns the string representation based on
// the current value.
func ConfigEntryToString(input interface{}) string {
	switch t := input.(type) {
	case bool:
		return strconv.FormatBool(t)
	case int64:
		return strconv.FormatInt(t, 10)
	case float64:
		return strconv.FormatFloat(t, 'f', 0, 64)
	default:
		return input.(string)
	}
}

func newApplicationClient(cf ConnectionFactory) *applicationsClient {
	return &applicationsClient{
		ConnectionFactory: cf,
	}
}

func resolveCharmURL(charmName string) (*charm.URL, error) {
	path, err := charm.EnsureSchema(charmName, charm.CharmHub)
	if err != nil {
		return nil, err
	}
	charmURL, err := charm.ParseURL(path)
	if err != nil {
		return nil, err
	}

	return charmURL, nil
}

func (c applicationsClient) ReadApplication(ctx context.Context, input *ReadApplicationInput) (*ReadApplicationResponse, error) {
	conn, err := c.GetConnection(ctx, &input.ModelUUID)
	if err != nil {
		return nil, err
	}

	applicationAPIClient := apiapplication.NewClient(conn)
	defer applicationAPIClient.Close()

	clientAPIClient := apiclient.NewClient(conn)
	defer clientAPIClient.Close()

	apps, err := applicationAPIClient.ApplicationsInfo([]names.ApplicationTag{names.NewApplicationTag(input.ApplicationName)})
	if err != nil {
		return nil, err
	}
	if len(apps) > 1 {
		return nil, fmt.Errorf("more than one result for application: %s", input.ApplicationName)
	}
	if len(apps) < 1 {
		return nil, fmt.Errorf("no results for application: %s", input.ApplicationName)
	}
	appInfo := apps[0].Result

	var appConstraints constraints.Value = constraints.Value{}
	// constraints do not apply to subordinate applications.
	if appInfo.Principal {
		queryConstraints, err := applicationAPIClient.GetConstraints(input.ApplicationName)
		if err != nil {
			return nil, err
		}
		if len(queryConstraints) != 1 {
			return nil, fmt.Errorf("expected one set of application constraints, received %d", len(queryConstraints))
		}
		appConstraints = queryConstraints[0]
	}

	status, err := clientAPIClient.Status(nil)
	if err != nil {
		return nil, err
	}
	var appStatus params.ApplicationStatus
	var exists bool
	if appStatus, exists = status.Applications[input.ApplicationName]; !exists {
		return nil, fmt.Errorf("no status returned for application: %s", input.ApplicationName)
	}

	unitCount := len(appStatus.Units)

	// NOTE: we are assuming that this charm comes from CharmHub
	charmURL, err := charm.ParseURL(appStatus.Charm)
	if err != nil {
		return nil, fmt.Errorf("failed to parse charm: %v", err)
	}

	returnedConf, err := applicationAPIClient.Get("master", input.ApplicationName)
	if err != nil {
		return nil, fmt.Errorf("failed to get app configuration %v", err)
	}

	conf := make(map[string]ConfigEntry, 0)
	if returnedConf.ApplicationConfig != nil {
		for k, v := range returnedConf.ApplicationConfig {
			// skip the trust value. We have an independent field for that
			if k == "trust" {
				continue
			}
			// The API returns the configuration entries as interfaces
			aux := v.(map[string]interface{})
			// set if we find the value key and this is not a default
			// value.
			if value, found := aux["value"]; found {
				conf[k] = ConfigEntry{
					Value:     value,
					IsDefault: aux["source"] == "default",
				}
			}
		}
		// repeat the same steps for charm config values
		for k, v := range returnedConf.CharmConfig {
			aux := v.(map[string]interface{})
			if value, found := aux["value"]; found {
				conf[k] = ConfigEntry{
					Value:     value,
					IsDefault: aux["source"] == "default",
				}
			}
		}
	}

	// trust field which has to be included into the configuration
	trustValue := false
	if returnedConf.ApplicationConfig != nil {
		aux, found := returnedConf.ApplicationConfig["trust"]
		if found {
			m := aux.(map[string]any)
			target, found := m["value"]
			if found {
				trustValue = target.(bool)
			}
		}
	}

	response := &ReadApplicationResponse{
		Name:        charmURL.Name,
		Channel:     appStatus.CharmChannel,
		Revision:    charmURL.Revision,
		Base:        appInfo.Base,
		Units:       unitCount,
		Trust:       trustValue,
		Config:      conf,
		Constraints: appConstraints,
		Principal:   appInfo.Principal,
		Status:      appStatus,
	}

	return response, nil
}

func (c applicationsClient) AreApplicationUnitsActiveIdle(ctx context.Context, input ReadApplicationInput) (bool, error) {
	log := log.FromContext(ctx)
	readAppResponse, err := c.ReadApplication(ctx, &input)
	if err != nil {
		return false, err
	}

	if len(readAppResponse.Status.Units) < 1 {
		return false, nil
	}

	for _, unit := range readAppResponse.Status.Units {
		if unit.WorkloadStatus.Status != "active" || unit.AgentStatus.Status != "idle" {
			log.Info("unit is not active/idle", "app status", readAppResponse.Status)
			return false, nil
		}
	}

	return true, nil
}
