//go:generate ../../../tools/readme_config_includer/generator
package groundwork

import (
	"context"
	_ "embed"
	"encoding/json"
	"errors"
	"fmt"
	"strconv"
	"strings"

	"github.com/gwos/tcg/sdk/clients"
	"github.com/gwos/tcg/sdk/log"
	"github.com/gwos/tcg/sdk/transit"
	"github.com/hashicorp/go-uuid"

	"github.com/influxdata/telegraf"
	"github.com/influxdata/telegraf/config"
	"github.com/influxdata/telegraf/plugins/common/slog"
	"github.com/influxdata/telegraf/plugins/outputs"
)

//go:embed sample.conf
var sampleConfig string

type metricMeta struct {
	group    string
	resource string
}

type Groundwork struct {
	Server              string          `toml:"url"`
	AgentID             string          `toml:"agent_id"`
	Username            config.Secret   `toml:"username"`
	Password            config.Secret   `toml:"password"`
	DefaultAppType      string          `toml:"default_app_type"`
	DefaultHost         string          `toml:"default_host"`
	DefaultServiceState string          `toml:"default_service_state"`
	GroupTag            string          `toml:"group_tag"`
	ResourceTag         string          `toml:"resource_tag"`
	Log                 telegraf.Logger `toml:"-"`
	client              clients.GWClient
}

func (*Groundwork) SampleConfig() string {
	return sampleConfig
}

func (g *Groundwork) Init() error {
	if g.Server == "" {
		return errors.New(`no "url" provided`)
	}
	if g.AgentID == "" {
		return errors.New(`no "agent_id" provided`)
	}
	if g.Username.Empty() {
		return errors.New(`no "username" provided`)
	}
	if g.Password.Empty() {
		return errors.New(`no "password" provided`)
	}
	if g.DefaultAppType == "" {
		return errors.New(`no "default_app_type" provided`)
	}
	if g.DefaultHost == "" {
		return errors.New(`no "default_host" provided`)
	}
	if g.ResourceTag == "" {
		return errors.New(`no "resource_tag" provided`)
	}
	if !validStatus(g.DefaultServiceState) {
		return errors.New(`invalid "default_service_state" provided`)
	}

	username, err := g.Username.Get()
	if err != nil {
		return fmt.Errorf("getting username failed: %w", err)
	}
	password, err := g.Password.Get()
	if err != nil {
		username.Destroy()
		return fmt.Errorf("getting password failed: %w", err)
	}
	g.client = clients.GWClient{
		AppName: "telegraf",
		AppType: g.DefaultAppType,
		GWConnection: &clients.GWConnection{
			HostName:           g.Server,
			UserName:           username.String(),
			Password:           password.String(),
			IsDynamicInventory: true,
		},
	}
	username.Destroy()
	password.Destroy()

	/* adapt SDK logger */
	log.Logger = slog.NewLogger(g.Log).WithGroup("tcg.sdk")

	return nil
}

func (g *Groundwork) Connect() error {
	err := g.client.Connect()
	if err != nil {
		return fmt.Errorf("could not login: %w", err)
	}
	return nil
}

func (g *Groundwork) Close() error {
	err := g.client.Disconnect()
	if err != nil {
		return fmt.Errorf("could not logout: %w", err)
	}
	return nil
}

func (g *Groundwork) Write(metrics []telegraf.Metric) error {
	groupMap := make(map[string][]transit.ResourceRef)
	resourceToServicesMap := make(map[string][]transit.MonitoredService)
	for _, metric := range metrics {
		meta, service := g.parseMetric(metric)
		resource := meta.resource
		resourceToServicesMap[resource] = append(resourceToServicesMap[resource], *service)

		group := meta.group
		if len(group) != 0 {
			resRef := transit.ResourceRef{
				Name: resource,
				Type: transit.ResourceTypeHost,
			}
			if refs, ok := groupMap[group]; ok {
				refs = append(refs, resRef)
				groupMap[group] = refs
			} else {
				groupMap[group] = []transit.ResourceRef{resRef}
			}
		}
	}

	groups := make([]transit.ResourceGroup, 0, len(groupMap))
	for groupName, refs := range groupMap {
		groups = append(groups, transit.ResourceGroup{
			GroupName: groupName,
			Resources: refs,
			Type:      transit.HostGroup,
		})
	}

	resources := make([]transit.MonitoredResource, 0, len(resourceToServicesMap))
	for resourceName, services := range resourceToServicesMap {
		resources = append(resources, transit.MonitoredResource{
			BaseResource: transit.BaseResource{
				BaseInfo: transit.BaseInfo{
					Name: resourceName,
					Type: transit.ResourceTypeHost,
				},
			},
			MonitoredInfo: transit.MonitoredInfo{
				Status:        transit.HostUp,
				LastCheckTime: transit.NewTimestamp(),
			},
			Services: services,
		})
	}

	traceToken, err := uuid.GenerateUUID()
	if err != nil {
		return err
	}
	requestJSON, err := json.Marshal(transit.ResourcesWithServicesRequest{
		Context: &transit.TracerContext{
			AppType:    g.DefaultAppType,
			AgentID:    g.AgentID,
			TraceToken: traceToken,
			TimeStamp:  transit.NewTimestamp(),
			Version:    transit.ModelVersion,
		},
		Resources: resources,
		Groups:    groups,
	})

	if err != nil {
		return err
	}

	_, err = g.client.SendResourcesWithMetrics(context.Background(), requestJSON)
	if err != nil {
		return fmt.Errorf("error while sending: %w", err)
	}

	return nil
}

func init() {
	outputs.Add("groundwork", func() telegraf.Output {
		return &Groundwork{
			GroupTag:            "group",
			ResourceTag:         "host",
			DefaultHost:         "telegraf",
			DefaultAppType:      "TELEGRAF",
			DefaultServiceState: string(transit.ServiceOk),
		}
	})
}

func (g *Groundwork) parseMetric(metric telegraf.Metric) (metricMeta, *transit.MonitoredService) {
	group, _ := metric.GetTag(g.GroupTag)

	resource := g.DefaultHost
	if v, ok := metric.GetTag(g.ResourceTag); ok {
		resource = v
	}

	service := metric.Name()
	if v, ok := metric.GetTag("service"); ok {
		service = v
	}

	unitType := string(transit.UnitCounter)
	if v, ok := metric.GetTag("unitType"); ok {
		unitType = v
	}

	lastCheckTime := transit.NewTimestamp()
	lastCheckTime.Time = metric.Time()
	serviceObject := transit.MonitoredService{
		BaseInfo: transit.BaseInfo{
			Name:       service,
			Type:       transit.ResourceTypeService,
			Owner:      resource,
			Properties: make(map[string]transit.TypedValue),
		},
		MonitoredInfo: transit.MonitoredInfo{
			Status:        transit.MonitorStatus(g.DefaultServiceState),
			LastCheckTime: lastCheckTime,
			NextCheckTime: lastCheckTime, // if not added, GW will make this as LastCheckTime + 5 mins
		},
		Metrics: nil,
	}

	knownKey := func(t string) bool {
		if strings.HasSuffix(t, "_cr") ||
			strings.HasSuffix(t, "_wn") ||
			t == "critical" ||
			t == "warning" ||
			t == g.GroupTag ||
			t == g.ResourceTag ||
			t == "service" ||
			t == "status" ||
			t == "message" ||
			t == "unitType" {
			return true
		}
		return false
	}

	for _, tag := range metric.TagList() {
		if knownKey(tag.Key) {
			continue
		}
		serviceObject.Properties[tag.Key] = *transit.NewTypedValue(tag.Value)
	}

	for _, field := range metric.FieldList() {
		if knownKey(field.Key) {
			continue
		}

		switch field.Value.(type) {
		case string, []byte:
			g.Log.Warnf("string values are not supported, skipping field %s: %q", field.Key, field.Value)
			continue
		}

		typedValue := transit.NewTypedValue(field.Value)
		if typedValue == nil {
			g.Log.Warnf("could not convert type %T, skipping field %s: %v", field.Value, field.Key, field.Value)
			continue
		}

		var thresholds []transit.ThresholdValue
		addCriticalThreshold := func(v interface{}) {
			if tv := transit.NewTypedValue(v); tv != nil {
				thresholds = append(thresholds, transit.ThresholdValue{
					SampleType: transit.Critical,
					Label:      field.Key + "_cr",
					Value:      tv,
				})
			}
		}
		addWarningThreshold := func(v interface{}) {
			if tv := transit.NewTypedValue(v); tv != nil {
				thresholds = append(thresholds, transit.ThresholdValue{
					SampleType: transit.Warning,
					Label:      field.Key + "_wn",
					Value:      tv,
				})
			}
		}
		if v, ok := metric.GetTag(field.Key + "_cr"); ok {
			if v, err := strconv.ParseFloat(v, 64); err == nil {
				addCriticalThreshold(v)
			}
		} else if v, ok := metric.GetTag("critical"); ok {
			if v, err := strconv.ParseFloat(v, 64); err == nil {
				addCriticalThreshold(v)
			}
		} else if v, ok := metric.GetField(field.Key + "_cr"); ok {
			addCriticalThreshold(v)
		}
		if v, ok := metric.GetTag(field.Key + "_wn"); ok {
			if v, err := strconv.ParseFloat(v, 64); err == nil {
				addWarningThreshold(v)
			}
		} else if v, ok := metric.GetTag("warning"); ok {
			if v, err := strconv.ParseFloat(v, 64); err == nil {
				addWarningThreshold(v)
			}
		} else if v, ok := metric.GetField(field.Key + "_wn"); ok {
			addWarningThreshold(v)
		}

		serviceObject.Metrics = append(serviceObject.Metrics, transit.TimeSeries{
			MetricName: field.Key,
			SampleType: transit.Value,
			Interval:   &transit.TimeInterval{EndTime: lastCheckTime},
			Value:      typedValue,
			Unit:       transit.UnitType(unitType),
			Thresholds: thresholds,
		})
	}

	if m, ok := metric.GetTag("message"); ok {
		serviceObject.LastPluginOutput = strings.ToValidUTF8(m, "?")
	} else if m, ok := metric.GetField("message"); ok {
		switch m := m.(type) {
		case string:
			serviceObject.LastPluginOutput = strings.ToValidUTF8(m, "?")
		case []byte:
			serviceObject.LastPluginOutput = strings.ToValidUTF8(string(m), "?")
		default:
			serviceObject.LastPluginOutput = strings.ToValidUTF8(fmt.Sprintf("%v", m), "?")
		}
	}

	func() {
		if s, ok := metric.GetTag("status"); ok && validStatus(s) {
			serviceObject.Status = transit.MonitorStatus(s)
			return
		}
		if s, ok := metric.GetField("status"); ok {
			status := g.DefaultServiceState
			switch s := s.(type) {
			case string:
				status = s
			case []byte:
				status = string(s)
			}
			if validStatus(status) {
				serviceObject.Status = transit.MonitorStatus(status)
				return
			}
		}
		status, err := transit.CalculateServiceStatus(&serviceObject.Metrics)
		if err != nil {
			g.Log.Infof("could not calculate service status, reverting to default_service_state: %v", err)
			status = transit.MonitorStatus(g.DefaultServiceState)
		}
		serviceObject.Status = status
	}()

	return metricMeta{resource: resource, group: group}, &serviceObject
}

func validStatus(status string) bool {
	switch transit.MonitorStatus(status) {
	case transit.ServiceOk, transit.ServiceWarning, transit.ServicePending, transit.ServiceScheduledCritical,
		transit.ServiceUnscheduledCritical, transit.ServiceUnknown:
		return true
	}
	return false
}
