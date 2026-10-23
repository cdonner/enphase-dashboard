package main

// Optional MQTT publishing with Home Assistant discovery (set MQTT_URL to enable).
// Home Assistant's MQTT integration then creates the sensors automatically: live power,
// today's energy, and per named appliance a "running" binary sensor, its power, and
// its energy today.

import (
	"encoding/json"
	"fmt"
	"regexp"
	"strings"
	"sync"
	"time"

	mqtt "github.com/eclipse/paho.mqtt.golang"
)

const (
	mqttStateEvery = 10 * time.Second
	mqttQoS        = 1
)

type MQTTConfig struct {
	URL, Username, Password string
	Topic                   string // base topic for state, default "enphase_dashboard"
	DiscoveryPrefix         string // Home Assistant discovery prefix, default "homeassistant"
}

type MQTT struct {
	s       *Server
	cfg     MQTTConfig
	client  mqtt.Client
	refresh chan struct{} // re-publish discovery (appliances changed)

	mu        sync.Mutex
	connected bool
	lastErr   *string
	published map[string]bool // appliance ids with discovery configs out
}

type MQTTStatus struct {
	Enabled   bool    `json:"enabled"`
	Connected bool    `json:"connected"`
	Broker    string  `json:"broker,omitempty"`
	Error     *string `json:"error"`
}

func NewMQTT(s *Server, cfg MQTTConfig) *MQTT {
	if cfg.Topic == "" {
		cfg.Topic = "enphase_dashboard"
	}
	if cfg.DiscoveryPrefix == "" {
		cfg.DiscoveryPrefix = "homeassistant"
	}
	return &MQTT{s: s, cfg: cfg, refresh: make(chan struct{}, 1), published: map[string]bool{}}
}

func (m *MQTT) status() MQTTStatus {
	if m == nil {
		return MQTTStatus{}
	}
	m.mu.Lock()
	defer m.mu.Unlock()
	return MQTTStatus{Enabled: true, Connected: m.connected, Broker: brokerHost(m.cfg.URL), Error: m.lastErr}
}

// brokerHost hides any credentials embedded in the URL.
func brokerHost(u string) string {
	if i := strings.LastIndex(u, "@"); i >= 0 {
		if j := strings.Index(u, "://"); j >= 0 {
			return u[:j+3] + u[i+1:]
		}
	}
	return u
}

func (m *MQTT) availability() string { return m.cfg.Topic + "/availability" }

func (m *MQTT) start() {
	url := strings.Replace(strings.Replace(m.cfg.URL, "mqtt://", "tcp://", 1), "mqtts://", "ssl://", 1)
	opts := mqtt.NewClientOptions().
		AddBroker(url).
		SetClientID("enphase-dashboard-"+newID()).
		SetUsername(m.cfg.Username).
		SetPassword(m.cfg.Password).
		SetWill(m.availability(), "offline", mqttQoS, true).
		SetAutoReconnect(true).
		SetConnectRetry(true).
		SetConnectRetryInterval(30 * time.Second).
		SetOnConnectHandler(func(mqtt.Client) {
			m.setState(true, nil)
			m.s.logf("MQTT connected to %s", brokerHost(m.cfg.URL))
			m.mu.Lock()
			m.published = map[string]bool{} // republish everything after a reconnect
			m.mu.Unlock()
			m.publishDiscovery()
			m.publish(m.availability(), "online", true)
			m.publishState()
		}).
		SetConnectionLostHandler(func(_ mqtt.Client, err error) {
			m.setState(false, err)
			m.s.logf("MQTT connection lost: %v", err)
		})
	m.client = mqtt.NewClient(opts)
	m.client.Connect() // retries in the background
	go m.loop()
}

func (m *MQTT) setState(connected bool, err error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	m.connected = connected
	m.lastErr = nil
	if err != nil {
		m.lastErr = ptr(err.Error())
	}
}

func (m *MQTT) loop() {
	t := time.NewTicker(mqttStateEvery)
	for {
		select {
		case <-t.C:
		case <-m.refresh:
			m.publishDiscovery()
		}
		if m.client.IsConnectionOpen() {
			m.publishState()
		}
	}
}

// appliancesChanged asks the loop to update discovery configs.
func (m *MQTT) appliancesChanged() {
	if m == nil {
		return
	}
	select {
	case m.refresh <- struct{}{}:
	default:
	}
}

func (m *MQTT) publish(topic string, payload any, retain bool) {
	var b []byte
	switch p := payload.(type) {
	case string:
		b = []byte(p)
	case []byte:
		b = p
	default:
		b, _ = json.Marshal(p)
	}
	tok := m.client.Publish(topic, mqttQoS, retain, b)
	go func() {
		if tok.WaitTimeout(10*time.Second) && tok.Error() != nil {
			m.setState(m.client.IsConnectionOpen(), tok.Error())
		}
	}()
}

var nonID = regexp.MustCompile(`[^a-z0-9_]+`)

func slug(s string) string { return strings.Trim(nonID.ReplaceAllString(strings.ToLower(s), "_"), "_") }

func (m *MQTT) nodeID() string {
	m.s.mu.Lock()
	defer m.s.mu.Unlock()
	if sn := deref(m.s.gwInfo.Serial); sn != "" {
		return "enphase_" + slug(sn)
	}
	return "enphase_dashboard"
}

func (m *MQTT) device(node string) map[string]any {
	return map[string]any{
		"identifiers":  []string{node},
		"name":         "Enphase dashboard",
		"manufacturer": "Enphase (via enphase-dashboard)",
		"model":        "IQ Gateway",
	}
}

type haEntity struct {
	component, object, name string
	fields                  map[string]any
}

// publishDiscovery sends retained Home Assistant discovery configs for the system
// sensors and every named appliance, and removes configs of forgotten appliances.
func (m *MQTT) publishDiscovery() {
	if !m.client.IsConnectionOpen() {
		return
	}
	node := m.nodeID()
	dev := m.device(node)
	state := m.cfg.Topic + "/state"
	power := func(key string) map[string]any {
		return map[string]any{"state_topic": state, "value_template": "{{ value_json." + key + " }}",
			"unit_of_measurement": "W", "device_class": "power", "state_class": "measurement"}
	}
	energy := func(key string) map[string]any {
		return map[string]any{"state_topic": state, "value_template": "{{ value_json." + key + " }}",
			"unit_of_measurement": "Wh", "device_class": "energy", "state_class": "total_increasing"}
	}
	entities := []haEntity{
		{"sensor", "production_power", "Solar production", power("production_w")},
		{"sensor", "consumption_power", "House consumption", power("consumption_w")},
		{"sensor", "grid_power", "Grid power (import +, export -)", power("grid_w")},
		{"sensor", "produced_today", "Solar produced today", energy("produced_today_wh")},
		{"sensor", "consumed_today", "Consumed today", energy("consumed_today_wh")},
		{"sensor", "imported_today", "Grid import today", energy("imported_today_wh")},
		{"sensor", "exported_today", "Grid export today", energy("exported_today_wh")},
	}

	m.s.mu.Lock()
	apps := append([]Appliance{}, m.s.appliances...)
	m.s.mu.Unlock()
	current := map[string]bool{}
	for _, a := range apps {
		if a.Ignored {
			continue
		}
		current[a.ID] = true
		topic := m.cfg.Topic + "/appliance/" + a.ID
		entities = append(entities,
			haEntity{"binary_sensor", "appliance_" + a.ID + "_running", a.Name, map[string]any{
				"state_topic": topic, "value_template": "{{ value_json.running }}", "device_class": "running",
				"payload_on": "ON", "payload_off": "OFF"}},
			haEntity{"sensor", "appliance_" + a.ID + "_power", a.Name + " power", map[string]any{
				"state_topic": topic, "value_template": "{{ value_json.power_w }}",
				"unit_of_measurement": "W", "device_class": "power", "state_class": "measurement"}},
			haEntity{"sensor", "appliance_" + a.ID + "_energy_today", a.Name + " energy today", map[string]any{
				"state_topic": topic, "value_template": "{{ value_json.energy_today_wh }}",
				"unit_of_measurement": "Wh", "device_class": "energy", "state_class": "total_increasing"}},
		)
	}
	for _, e := range entities {
		cfg := map[string]any{
			"name": e.name, "unique_id": node + "_" + e.object, "object_id": node + "_" + e.object,
			"device": dev, "availability_topic": m.availability(),
		}
		for k, v := range e.fields {
			cfg[k] = v
		}
		m.publish(fmt.Sprintf("%s/%s/%s/%s/config", m.cfg.DiscoveryPrefix, e.component, node, e.object), cfg, true)
	}

	// Forgotten appliances: an empty retained config removes the entity in Home Assistant.
	m.mu.Lock()
	for id := range m.published {
		if !current[id] {
			for _, o := range []struct{ comp, obj string }{
				{"binary_sensor", "_running"}, {"sensor", "_power"}, {"sensor", "_energy_today"},
			} {
				m.publish(fmt.Sprintf("%s/%s/%s/appliance_%s%s/config", m.cfg.DiscoveryPrefix, o.comp, node, id, o.obj), "", true)
			}
			m.publish(m.cfg.Topic+"/appliance/"+id, "", true)
		}
	}
	m.published = current
	m.mu.Unlock()
}

type applianceState struct {
	Running       string   `json:"running"` // "ON" / "OFF"
	PowerW        float64  `json:"power_w"`
	EnergyTodayWh float64  `json:"energy_today_wh"`
	LastStart     *string  `json:"last_start"`
	LastEnd       *string  `json:"last_end"`
	Name          string   `json:"name"`
	Signature     []string `json:"signature"`
}

// publishState sends the system values and every named appliance's state.
func (m *MQTT) publishState() {
	s := m.s
	s.mu.Lock()
	today := dayKey(time.Now())
	st := map[string]any{"updated": time.Now().Format(time.RFC3339)}
	if s.live != nil {
		st["production_w"] = round1(s.live.ProductionW)
		if s.live.ConsumptionW != nil {
			st["consumption_w"] = round1(*s.live.ConsumptionW)
		}
		if s.live.NetW != nil {
			st["grid_w"] = round1(*s.live.NetW)
		}
	}
	d := s.publicDaily(today)
	for k, v := range map[string]*float64{
		"produced_today_wh": d.Produced, "consumed_today_wh": d.Consumed,
		"imported_today_wh": d.Imported, "exported_today_wh": d.Exported,
	} {
		if v != nil {
			st[k] = *v
		}
	}

	// Appliance states from today's runs plus the runs on right now.
	states := map[string]*applianceState{}
	for _, a := range s.appliances {
		if a.Ignored {
			continue
		}
		var sigs []string
		for _, sg := range a.Signatures {
			d := describe(sg.DP, sg.DQ)
			sigs = append(sigs, fmt.Sprintf("%.0f W %s %s", d.PowerW, d.Legs, d.Character))
		}
		states[a.ID] = &applianceState{Running: "OFF", Name: a.Name, Signature: sigs}
	}
	now := nowMs()
	iso := func(ms int64) *string { return ptr(time.UnixMilli(ms).Format(time.RFC3339)) }
	add := func(r Run) {
		app := s.applianceFor(r)
		if app == nil {
			return
		}
		a := states[app.ID]
		if a == nil { // ignored
			return
		}
		end := now
		if r.End != nil {
			end = *r.End
			if a.LastEnd == nil || *a.LastEnd < *iso(end) {
				a.LastEnd = iso(end)
			}
		} else {
			a.Running = "ON"
			a.PowerW += r.power()
		}
		a.EnergyTodayWh += r.power() * float64(end-max(r.Start, startOfDay(time.Now()).UnixMilli())) / 3_600_000
		if a.LastStart == nil || *a.LastStart < *iso(r.Start) {
			a.LastStart = iso(r.Start)
		}
	}
	for _, r := range s.loadDayRuns(today) {
		add(r)
	}
	for _, r := range s.detector.open {
		add(r)
	}
	s.mu.Unlock()

	m.publish(m.cfg.Topic+"/state", st, true)
	for id, a := range states {
		a.EnergyTodayWh = round1(a.EnergyTodayWh)
		a.PowerW = round1(a.PowerW)
		m.publish(m.cfg.Topic+"/appliance/"+id, a, true)
	}
}

func round1(v float64) float64 { return float64(int64(v*10+0.5*sign(v))) / 10 }

func sign(v float64) float64 {
	if v < 0 {
		return -1
	}
	return 1
}
