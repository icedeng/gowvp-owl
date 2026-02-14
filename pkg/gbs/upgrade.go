package gbs

import (
	"context"
	"encoding/xml"
	"errors"
	"fmt"
	"strings"
	"time"

	"github.com/gowvp/owl/pkg/gbs/sip"
)

type UpgradeInput struct {
	DeviceID     string
	ChannelID    string
	Firmware     string
	FileURL      string
	Manufacturer string
	SessionID    string
	Timeout      time.Duration
}

type UpgradeOutput struct {
	SN       int    `json:"sn"`
	DeviceID string `json:"device_id"`
	Channel  string `json:"channel"`
	Result   string `json:"result"`
}

type deviceControlUpgradeRequest struct {
	XMLName    xml.Name `xml:"Control"`
	CmdType    string   `xml:"CmdType"`
	SN         int      `xml:"SN"`
	DeviceID   string   `xml:"DeviceID"`
	DeviceInfo struct {
		Firmware     string `xml:"Firmware"`
		FileURL      string `xml:"FileURL"`
		Manufacturer string `xml:"Manufacturer"`
		SessionID    string `xml:"SessionID,omitempty"`
	} `xml:"DeviceUpgrade"`
}

// Upgrade 执行设备软件升级（GB/T 28181-2022 9.13，A.2.3.1.12）。
func (g *GB28181API) Upgrade(_ context.Context, in *UpgradeInput) (*UpgradeOutput, error) {
	if in == nil || in.DeviceID == "" || in.ChannelID == "" {
		return nil, errors.New("invalid upgrade input")
	}
	if err := g.requireGBVersionAtLeast(in.DeviceID, gbVersion2022, "设备软件升级(9.13)"); err != nil {
		return nil, err
	}
	if strings.TrimSpace(in.Firmware) == "" || strings.TrimSpace(in.FileURL) == "" || strings.TrimSpace(in.Manufacturer) == "" {
		return nil, errors.New("firmware/file_url/manufacturer are required")
	}

	ipc, ok := g.svr.memoryStorer.Load(in.DeviceID)
	if !ok || !ipc.IsOnline {
		return nil, ErrDeviceOffline
	}
	ch, ok := g.svr.memoryStorer.GetChannel(in.DeviceID, in.ChannelID)
	if !ok {
		return nil, ErrChannelNotExist
	}
	if in.Timeout <= 0 {
		in.Timeout = 8 * time.Second
	}

	sn := g.nextControlSN()
	req := deviceControlUpgradeRequest{
		CmdType:  ptzCmdTypeDeviceControl,
		SN:       sn,
		DeviceID: in.ChannelID,
	}
	req.DeviceInfo.Firmware = strings.TrimSpace(in.Firmware)
	req.DeviceInfo.FileURL = strings.TrimSpace(in.FileURL)
	req.DeviceInfo.Manufacturer = strings.TrimSpace(in.Manufacturer)
	req.DeviceInfo.SessionID = strings.TrimSpace(in.SessionID)

	body, err := sip.XMLEncode(req)
	if err != nil {
		return nil, err
	}

	waitKey := fmt.Sprintf("%s:%d", in.DeviceID, sn)
	pending := &pendingDeviceControl{wait: make(chan *deviceControlResponse, 1)}
	g.pendingDeviceControl.Store(waitKey, pending)
	defer g.pendingDeviceControl.Delete(waitKey)

	tx, err := g.svr.wrapRequest(ch, sip.MethodMessage, &sip.ContentTypeXML, body)
	if err != nil {
		return nil, err
	}
	if _, err = sipResponse(tx); err != nil {
		return nil, err
	}

	timer := time.NewTimer(in.Timeout)
	defer timer.Stop()
	select {
	case resp := <-pending.wait:
		result := strings.ToUpper(strings.TrimSpace(resp.Result))
		if result == "" {
			result = "OK"
		}
		if result != "OK" {
			return nil, fmt.Errorf("device upgrade failed: %s", resp.Result)
		}
		return &UpgradeOutput{
			SN:       sn,
			DeviceID: in.DeviceID,
			Channel:  in.ChannelID,
			Result:   result,
		}, nil
	case <-timer.C:
		return nil, errors.New("wait device upgrade response timeout")
	}
}
