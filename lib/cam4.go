package lib

import (
	"bytes"
	"encoding/json"
	"fmt"
	"io"
	"regexp"
	"strings"
)

// Cam4Checker implements a checker for CAM4
type Cam4Checker struct{ CheckerCommon }

var _ Checker = &Cam4Checker{}

// Cam4ModelIDRegexp is a regular expression to check model IDs
var Cam4ModelIDRegexp = regexp.MustCompile(`^[a-z0-9_]+$`)

var cam4ModelRegex = regexp.MustCompile(`^(?:https?://)?cam4\.com/([A-Za-z0-9_]+)(?:[/?].*)?$`)

// Cam4CanonicalModelID preprocesses model ID string to canonical for CAM4 form
func Cam4CanonicalModelID(name string) string {
	m := cam4ModelRegex.FindStringSubmatch(name)
	if len(m) == 2 {
		name = m[1]
	}
	return strings.ToLower(name)
}

type cam4Model struct {
	Nickname string `json:"nickname"`
	ThumbBig string `json:"thumb_big"`
}

type cam4Response struct {
	Username string `json:"username"`
	Status   string `json:"status"`
}

// CheckStatusSingle checks CAM4 model status
func (c *Cam4Checker) CheckStatusSingle(modelID string) StatusKind {
	url := fmt.Sprintf("https://api.pinklabel.com/api/v1/cams/profile/%s.json", modelID)
	addr, resp := c.doGetRequest(url)
	if resp == nil {
		return StatusUnknown
	}
	defer CloseBody(resp.Body)
	if resp.StatusCode == 404 {
		return StatusNotFound
	}
	buf := bytes.Buffer{}
	_, err := buf.ReadFrom(resp.Body)
	if err != nil {
		Lerr("[%v] cannot read response for model %s, %v", addr, modelID, err)
		return StatusUnknown
	}
	decoder := json.NewDecoder(io.NopCloser(bytes.NewReader(buf.Bytes())))
	parsed := &cam4Response{}
	err = decoder.Decode(parsed)
	if err != nil {
		Lerr("[%v] cannot parse response for model %s, %v", addr, modelID, err)
		if c.Dbg {
			Ldbg("response: %s", buf.String())
		}
		return StatusUnknown
	}
	return cam4RoomStatus(parsed.Status)
}

func cam4RoomStatus(roomStatus string) StatusKind {
	switch roomStatus {
	case "online":
		return StatusOnline
	case "offline":
		return StatusOffline
	}
	Lerr("cannot parse room status \"%s\"", roomStatus)
	return StatusUnknown
}

// checkEndpoint returns CAM4 online models on the endpoint
func (c *Cam4Checker) checkEndpoint(endpoint string) (onlineModels map[string]StatusKind, images map[string]string, err error) {
	client := c.clientsLoop.nextClient()
	onlineModels = map[string]StatusKind{}
	images = map[string]string{}
	resp, buf, err := onlineQuery(endpoint, client, c.Headers)
	if err != nil {
		return nil, nil, fmt.Errorf("cannot send a query, %v", err)
	}
	if resp.StatusCode != 200 {
		return nil, nil, fmt.Errorf("query status, %d", resp.StatusCode)
	}
	decoder := json.NewDecoder(io.NopCloser(bytes.NewReader(buf.Bytes())))
	var parsed []cam4Model
	err = decoder.Decode(&parsed)
	if err != nil {
		if c.Dbg {
			Ldbg("response: %s", buf.String())
		}
		return nil, nil, fmt.Errorf("cannot parse response, %v", err)
	}
	for _, m := range parsed {
		modelID := strings.ToLower(m.Nickname)
		onlineModels[modelID] = StatusOnline
		images[modelID] = m.ThumbBig
	}
	return
}

// CheckStatusesMany returns CAM4 online models
func (c *Cam4Checker) CheckStatusesMany(QueryModelList, CheckMode) (onlineModels map[string]StatusKind, images map[string]string, err error) {
	return checkEndpoints(c, c.UsersOnlineEndpoints, c.Dbg)
}

// Start starts a daemon
func (c *Cam4Checker) Start()                 { c.startFullCheckerDaemon(c) }
func (c *Cam4Checker) createUpdater() Updater { return c.createFullUpdater(c) }
