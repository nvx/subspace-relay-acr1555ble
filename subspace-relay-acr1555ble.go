package main

import (
	"context"
	"encoding/hex"
	"errors"
	"flag"
	"github.com/eclipse/paho.golang/paho"
	"github.com/nvx/go-acr1555ble"
	"github.com/nvx/go-rfid"
	"github.com/nvx/go-subspace-relay"
	"github.com/nvx/go-subspace-relay-logger"
	subspacerelaypb "github.com/nvx/subspace-relay"
	"log/slog"
	"os"
	"os/signal"
	"strings"
	"syscall"
	"tinygo.org/x/bluetooth"
)

// This can be set at build time using the following go build command:
// go build -ldflags="-X 'main.defaultBrokerURL=mqtts://user:pass@example.com:1234'"
var defaultBrokerURL string

func main() {
	ctx := context.Background()
	var (
		name       = flag.String("name", "", "MAC address or BLE name of the ACR1555 reader to connect to. Leave empty to connect to the first reader found with a name starting with ACR1555U or * to scan")
		sam        = flag.Bool("sam", false, "Connect to SAM slot instead of PICC")
		brokerFlag = flag.String("broker-url", "", "MQTT Broker URL")
	)
	flag.Parse()

	srlog.InitLogger("subspace-relay-acr1555ble")

	brokerURL := subspacerelay.NotZero(*brokerFlag, os.Getenv("BROKER_URL"), defaultBrokerURL)
	if brokerURL == "" {
		slog.ErrorContext(ctx, "No broker URI specified, either specify as a flag or set the BROKER_URI environment variable")
		flag.Usage()
		os.Exit(2)
	}

	adapter := bluetooth.DefaultAdapter
	err := adapter.Enable()
	if err != nil {
		slog.ErrorContext(ctx, "Error connecting to bluetooth adapter", rfid.ErrorAttrs(err))
		os.Exit(1)
	}

	res, err := scan(ctx, adapter, *name)
	if err != nil {
		slog.ErrorContext(ctx, "Error scanning", rfid.ErrorAttrs(err))
		os.Exit(1)
	}

	card, closer, err := connectCard(ctx, adapter, res.Address, *sam)
	if err != nil {
		slog.ErrorContext(ctx, "Error connecting to PCSC card", rfid.ErrorAttrs(err))
		os.Exit(1)
	}
	defer closer()

	atr, err := card.ATR()
	if err != nil {
		slog.ErrorContext(ctx, "Error getting card ATR", rfid.ErrorAttrs(err))
		os.Exit(1)
	}

	if len(atr) > 0 {
		slog.InfoContext(ctx, "Got ATR", slog.String("atr", strings.ToUpper(hex.EncodeToString(atr))))
	}

	h := &handler{
		card: card,
		relayInfo: &subspacerelaypb.RelayInfo{
			SupportedPayloadTypes: []subspacerelaypb.PayloadType{subspacerelaypb.PayloadType_PAYLOAD_TYPE_PCSC_READER, subspacerelaypb.PayloadType_PAYLOAD_TYPE_PCSC_READER_CONTROL},
			ConnectionType:        subspacerelaypb.ConnectionType_CONNECTION_TYPE_PCSC,
			Atr:                   atr,
			DeviceName:            card.DeviceName(),
			DeviceAddress:         res.Address.MAC[:],
			Rssi:                  int32(res.RSSI),
		},
	}

	m, err := subspacerelay.New(ctx, brokerURL, "")
	if err != nil {
		slog.ErrorContext(ctx, "Error connecting to server", rfid.ErrorAttrs(err))
		os.Exit(1)
	}

	m.RegisterHandler(h)

	slog.InfoContext(ctx, "Connected, provide the relay_id to your friendly neighbourhood RFID hacker", slog.String("relay_id", m.RelayID))

	interruptChannel := make(chan os.Signal, 10)
	signal.Notify(interruptChannel, syscall.SIGINT, syscall.SIGTERM)
	defer signal.Stop(interruptChannel)

	<-interruptChannel
	err = m.Close()
	if err != nil {
		slog.ErrorContext(ctx, "Error closing relay", rfid.ErrorAttrs(err))
	}
}

type handler struct {
	card      *acr1555ble.Card
	relayInfo *subspacerelaypb.RelayInfo
}

func (h *handler) HandleMQTT(ctx context.Context, r *subspacerelay.SubspaceRelay, p *paho.Publish) bool {
	req, err := r.Parse(ctx, p)
	if err != nil {
		slog.ErrorContext(ctx, "Error parsing request message", rfid.ErrorAttrs(err))
		return false
	}

	switch msg := req.Message.(type) {
	case *subspacerelaypb.Message_Payload:
		err = r.HandlePayload(ctx, p.Properties, msg.Payload, h.handlePayload, h.relayInfo.SupportedPayloadTypes...)
	case *subspacerelaypb.Message_RequestRelayInfo:
		err = r.SendReply(ctx, p.Properties, &subspacerelaypb.Message{Message: &subspacerelaypb.Message_RelayInfo{
			RelayInfo: h.relayInfo,
		}})
	default:
		err = errors.New("unsupported message")
	}
	if err != nil {
		slog.ErrorContext(ctx, "Error handling request", rfid.ErrorAttrs(err))
		return false
	}
	return true
}

func (h *handler) handlePayload(ctx context.Context, payload *subspacerelaypb.Payload) (_ []byte, err error) {
	defer rfid.DeferWrap(ctx, &err)

	if payload.PayloadType == subspacerelaypb.PayloadType_PAYLOAD_TYPE_PCSC_READER_CONTROL {
		if payload.Control == nil || *payload.Control > 0xFFFF {
			err = errors.New("invalid control payload")
			return
		}
		return h.card.Control(ctx, uint16(*payload.Control), payload.Payload)
	}

	return h.card.Exchange(ctx, payload.Payload)
}

func connectCard(ctx context.Context, adapter *bluetooth.Adapter, address bluetooth.Address, sam bool) (_ *acr1555ble.Card, _ func(), err error) {
	defer rfid.DeferWrap(ctx, &err)

	reader, err := acr1555ble.New(ctx, adapter, address)
	if err != nil {
		return
	}

	protocol := acr1555ble.ProtocolPICC
	if sam {
		protocol = acr1555ble.ProtocolT1
	}

	card, err := reader.Connect(ctx, protocol)
	if err != nil {
		_ = reader.Close()
		return
	}

	closer := func() {
		err := card.Close()
		if err != nil {
			slog.ErrorContext(ctx, "Error disconnecting from card", rfid.ErrorAttrs(err))
		}
		err = reader.Close()
		if err != nil {
			slog.ErrorContext(ctx, "Error disconnecting from reader", rfid.ErrorAttrs(err))
		}
	}

	return card, closer, nil
}
