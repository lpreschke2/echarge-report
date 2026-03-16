package carinfo

import (
	"crypto/tls"
	"echarge-report/pkg/config"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"strings"
	"time"

	"github.com/gorilla/websocket"
	"github.com/spf13/viper"
)

type HomeAssistantProvider struct {
	apiURL, token, sensorID string
	client                  *http.Client
}

type stateResponse struct {
	State      string `json:"state"`
	Attributes struct {
		UnitOfMeasurement string `json:"unit_of_measurement"`
	} `json:"attributes"`
}

func NewHomeAssistantProvider() *HomeAssistantProvider {
	return &HomeAssistantProvider{
		apiURL:   strings.TrimRight(viper.GetString(config.KeyHAAPI), "/"),
		token:    viper.GetString(config.KeyHAToken),
		sensorID: viper.GetString(config.KeyHAMilageSensor),
		client:   &http.Client{Transport: &http.Transport{TLSClientConfig: &tls.Config{InsecureSkipVerify: true}}},
	}
}

func (p *HomeAssistantProvider) GetType() string             { return TypeHomeAssistant }
func (p *HomeAssistantProvider) GetMileage() (string, error) { return p.GetMileageAt(time.Now()) }

func (p *HomeAssistantProvider) GetMileageAt(t time.Time) (string, error) {
	if p.apiURL == "" || p.token == "" || p.sensorID == "" {
		return "unknown", fmt.Errorf("missing configuration")
	}
	if result, err := p.fromHistory(t); err == nil {
		return result, nil
	}
	if result, err := p.fromStatistics(t); err == nil {
		return result, nil
	}
	return "unknown", fmt.Errorf("no data available for %s", t.Format("02.01.2006"))
}

func (p *HomeAssistantProvider) doGet(path string) ([]byte, error) {
	req, _ := http.NewRequest("GET", p.apiURL+path, nil)
	req.Header.Set("Authorization", "Bearer "+p.token)
	resp, err := p.client.Do(req)
	if err != nil {
		return nil, err
	}
	defer resp.Body.Close()
	body, _ := io.ReadAll(resp.Body)
	if resp.StatusCode != 200 {
		return nil, fmt.Errorf("status %d", resp.StatusCode)
	}
	return body, nil
}

func (p *HomeAssistantProvider) fromHistory(t time.Time) (string, error) {
	path := fmt.Sprintf("/api/history/period/%s?filter_entity_id=%s&end_time=%s",
		url.PathEscape(t.Add(-7*24*time.Hour).UTC().Format(time.RFC3339)),
		url.QueryEscape(p.sensorID),
		url.QueryEscape(t.UTC().Format(time.RFC3339)))

	body, err := p.doGet(path)
	if err != nil {
		return "", err
	}
	var history [][]stateResponse
	if json.Unmarshal(body, &history) != nil || len(history) == 0 || len(history[0]) == 0 {
		return "", fmt.Errorf("no data")
	}
	s := history[0][len(history[0])-1]
	return p.format(s.State, s.Attributes.UnitOfMeasurement)
}

func (p *HomeAssistantProvider) fromStatistics(t time.Time) (string, error) {
	wsURL := strings.NewReplacer("https://", "wss://", "http://", "ws://").Replace(p.apiURL)
	conn, _, err := (&websocket.Dialer{TLSClientConfig: &tls.Config{InsecureSkipVerify: true}}).Dial(wsURL+"/api/websocket", nil)
	if err != nil {
		return "", err
	}
	defer conn.Close()

	var msg map[string]interface{}
	conn.ReadJSON(&msg)
	conn.WriteJSON(map[string]string{"type": "auth", "access_token": p.token})
	conn.ReadJSON(&msg)
	if msg["type"] != "auth_ok" {
		return "", fmt.Errorf("auth failed")
	}

	start := time.Date(t.Year(), t.Month(), t.Day(), 0, 0, 0, 0, t.Location()).UTC()
	conn.WriteJSON(map[string]interface{}{
		"id": 1, "type": "recorder/statistics_during_period", "period": "hour",
		"start_time": start.Format(time.RFC3339), "end_time": start.Add(24 * time.Hour).Format(time.RFC3339),
		"statistic_ids": []string{p.sensorID},
	})

	var result map[string]interface{}
	conn.ReadJSON(&result)

	data, _ := result["result"].(map[string]interface{})
	stats, _ := data[p.sensorID].([]interface{})
	if len(stats) == 0 {
		return "", fmt.Errorf("no data")
	}
	last := stats[len(stats)-1].(map[string]interface{})
	for _, k := range []string{"state", "max", "mean"} {
		if v, ok := last[k].(float64); ok {
			return p.format(fmt.Sprintf("%.0f", v), "")
		}
	}
	return "", fmt.Errorf("no value")
}

func (p *HomeAssistantProvider) format(state, unit string) (string, error) {
	if state == "" || state == "unavailable" || state == "unknown" {
		return "", fmt.Errorf("invalid state")
	}
	if unit == "" {
		if body, err := p.doGet("/api/states/" + p.sensorID); err == nil {
			var s stateResponse
			json.Unmarshal(body, &s)
			unit = s.Attributes.UnitOfMeasurement
		}
	}
	if unit != "" {
		return state + " " + unit, nil
	}
	return state, nil
}
