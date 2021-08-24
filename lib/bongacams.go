package lib

import (
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
	"io/ioutil"
	"strings"
)

// BongaCamsChecker implements a checker for BongaCams
type BongaCamsChecker struct{ CheckerCommon }

var _ Checker = &BongaCamsChecker{}

type bongacamsModel struct {
	Username      string `json:"username"`
	ProfileImages struct {
		ThumbnailImageMediumLive string `json:"thumbnail_image_medium_live"`
	} `json:"profile_images"`
}

// CheckStatusSingle checks BongaCams model status
func (c *BongaCamsChecker) CheckStatusSingle(modelID string) StatusKind {
	code := c.queryStatusCode(fmt.Sprintf("https://en.bongacams.com/%s", modelID))
	switch code {
	case 200:
		return StatusOnline
	case 302:
		return StatusOffline
	case 404:
		return StatusNotFound
	}
	return StatusUnknown
}

// checkEndpoint returns BongaCams online models on the endpoint
func (c *BongaCamsChecker) checkEndpoint(endpoint string) (onlineModels map[string]StatusKind, images map[string]string, err error) {
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
	decoder := json.NewDecoder(ioutil.NopCloser(bytes.NewReader(buf.Bytes())))
	var parsed []bongacamsModel
	err = decoder.Decode(&parsed)
	if err != nil {
		if c.Dbg {
			Ldbg("response: %s", buf.String())
		}
		return nil, nil, fmt.Errorf("cannot parse response, %v", err)
	}

	if len(parsed) == 0 {
		return nil, nil, errors.New("zero online models reported")
	}

	for _, m := range parsed {
		modelID := strings.ToLower(m.Username)
		onlineModels[modelID] = StatusOnline
		images[modelID] = "https:" + m.ProfileImages.ThumbnailImageMediumLive
	}
	return
}

// CheckStatusesMany returns BongaCams online models
func (c *BongaCamsChecker) CheckStatusesMany(QueryModelList, CheckMode) (onlineModels map[string]StatusKind, images map[string]string, err error) {
	return checkEndpoints(c, c.UsersOnlineEndpoints, c.Dbg)
}

// Start starts a daemon
func (c *BongaCamsChecker) Start()                 { c.startFullCheckerDaemon(c) }
func (c *BongaCamsChecker) createUpdater() Updater { return c.createFullUpdater(c) }
