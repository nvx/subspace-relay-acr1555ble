package main

import (
	"context"
	subspacerelay "github.com/nvx/go-subspace-relay"
	"log/slog"
	"regexp"
	"strings"
	"tinygo.org/x/bluetooth"
)

var validMAC = regexp.MustCompile(`^([0-9A-Fa-f]{2}[:-]){5}([0-9A-Fa-f]{2})$`)

func scan(ctx context.Context, adapter *bluetooth.Adapter, device string) (_ bluetooth.ScanResult, err error) {
	defer subspacerelay.DeferWrap(&err)

	ch := make(chan bluetooth.ScanResult, 1)
	writeRes := func(res bluetooth.ScanResult) {
		_ = adapter.StopScan()
		select {
		case ch <- res:
		default:
		}
	}
	switch {
	case validMAC.MatchString(device):
		var mac bluetooth.MAC
		mac, err = bluetooth.ParseMAC(device)
		if err != nil {
			return
		}
		err = adapter.Scan(func(adapter *bluetooth.Adapter, res bluetooth.ScanResult) {
			if res.LocalName() == "" {
				return
			}

			if res.Address.String() == mac.String() {
				writeRes(res)
			}
		})
		if err != nil {
			return
		}
	case device == "*":
		slog.InfoContext(ctx, "Starting in scan only mode")
		err = adapter.Scan(func(adapter *bluetooth.Adapter, res bluetooth.ScanResult) {
			slog.InfoContext(ctx, "Found BLE peripheral", slog.String("local_name", res.LocalName()), slog.String("mac", res.Address.String()))
		})
		if err != nil {
			return
		}
	case device == "":
		err = adapter.Scan(func(adapter *bluetooth.Adapter, res bluetooth.ScanResult) {
			if strings.HasPrefix(res.LocalName(), "ACR1555U") {
				writeRes(res)
			}
		})
		if err != nil {
			return
		}
	default:
		err = adapter.Scan(func(adapter *bluetooth.Adapter, res bluetooth.ScanResult) {
			if res.LocalName() == device {
				writeRes(res)
			}
		})
		if err != nil {
			return
		}
	}

	select {
	case res := <-ch:
		slog.InfoContext(ctx, "Found BLE peripheral", slog.String("local_name", res.LocalName()), slog.String("mac", res.Address.String()), slog.Int("rssi", int(res.RSSI)))
		return res, nil
	case <-ctx.Done():
		_ = adapter.StopScan()
		err = context.Cause(ctx)
		return
	}
}
