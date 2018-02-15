package globe

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"strings"
	"time"

	"github.com/nyaruka/courier"
	"github.com/nyaruka/courier/handlers"
	"github.com/nyaruka/courier/utils"
	"github.com/nyaruka/gocommon/urns"
)

var maxMsgLength = 160
var sendURL = "https://devapi.globelabs.com.ph/smsmessaging/v1/outbound/%s/requests"

const (
	configPassphrase = "passphrase"
	configAppSecret  = "app_secret"
	configAppID      = "app_id"
)

func init() {
	courier.RegisterHandler(newHandler())
}

type handler struct {
	handlers.BaseHandler
}

func newHandler() courier.ChannelHandler {
	return &handler{handlers.NewBaseHandler(courier.ChannelType("GL"), "Globe Labs")}
}

// Initialize is called by the engine once everything is loaded
func (h *handler) Initialize(s courier.Server) error {
	h.SetServer(s)
	return s.AddHandlerRoute(h, http.MethodPost, "receive", h.ReceiveMessage)
}

// {
//	"inboundSMSMessageList":{
//		"inboundSMSMessage":[
//		   {
//			  "dateTime":"Fri Nov 22 2013 12:12:13 GMT+0000 (UTC)",
//			  "destinationAddress":"tel:21581234",
//			  "messageId":null,
//			  "message":"Hello",
//			  "resourceURL":null,
//			  "senderAddress":"tel:+639171234567"
//		   }
//		 ],
//		 "numberOfMessagesInThisBatch":1,
//		 "resourceURL":null,
//		 "totalNumberOfPendingMessages":null
//	 }
// }
type moMsg struct {
	InboundSMSMessageList struct {
		InboundSMSMessage []struct {
			DateTime           string `json:"dateTime"`
			DestinationAddress string `json:"destinationAddress"`
			MessageID          string `json:"messageId"`
			Message            string `json:"message"`
			SenderAddress      string `json:"senderAddress"`
		} `json:"inboundSMSMessage"`
	} `json:"inboundSMSMessageList"`
}

// ReceiveMessage is our HTTP handler function for incoming messages
func (h *handler) ReceiveMessage(ctx context.Context, c courier.Channel, w http.ResponseWriter, r *http.Request) ([]courier.Event, error) {
	glRequest := &moMsg{}
	err := handlers.DecodeAndValidateJSON(glRequest, r)
	if err != nil {
		return nil, courier.WriteAndLogRequestError(ctx, w, r, c, err)
	}

	if len(glRequest.InboundSMSMessageList.InboundSMSMessage) == 0 {
		return nil, courier.WriteAndLogRequestIgnored(ctx, w, r, c, "no messages, ignored")
	}

	events := make([]courier.Event, 0, 1)
	msgs := make([]courier.Msg, 0, 1)

	// parse each inbound message
	for _, glMsg := range glRequest.InboundSMSMessageList.InboundSMSMessage {
		// parse our date from format: "Fri Nov 22 2013 12:12:13 GMT+0000 (UTC)"
		date, err := time.Parse("Mon Jan 2 2006 15:04:05 GMT+0000 (UTC)", glMsg.DateTime)
		if err != nil {
			return nil, courier.WriteAndLogRequestError(ctx, w, r, c, err)
		}

		if !strings.HasPrefix(glMsg.SenderAddress, "tel:") {
			return nil, courier.WriteAndLogRequestError(ctx, w, r, c, fmt.Errorf("invalid 'senderAddress' parameter"))
		}

		urn := urns.NewTelURNForCountry(glMsg.SenderAddress[4:], c.Country())
		msg := h.Backend().NewIncomingMsg(c, urn, glMsg.Message).WithExternalID(glMsg.MessageID).WithReceivedOn(date)

		err = h.Backend().WriteMsg(ctx, msg)
		if err != nil {
			return nil, err
		}

		events = append(events, msg)
		msgs = append(msgs, msg)
	}

	return events, courier.WriteMsgSuccess(ctx, w, r, msgs)
}

// {
//	  "address": "250788383383",
//    "message": "hello world",
//    "passphrase": "my passphrase",
//    "app_id": "my app id",
//    "app_secret": "my app secret"
// }
type mtMsg struct {
	Address    string `json:"address"`
	Message    string `json:"message"`
	Passphrase string `json:"passphrase"`
	AppID      string `json:"app_id"`
	AppSecret  string `json:"app_secret"`
}

// SendMsg sends the passed in message, returning any error
func (h *handler) SendMsg(ctx context.Context, msg courier.Msg) (courier.MsgStatus, error) {
	appID := msg.Channel().StringConfigForKey(configAppID, "")
	if appID == "" {
		return nil, fmt.Errorf("Missing 'app_id' config for GL channel")
	}

	appSecret := msg.Channel().StringConfigForKey(configAppSecret, "")
	if appSecret == "" {
		return nil, fmt.Errorf("Missing 'app_secret' config for GL channel")
	}

	passphrase := msg.Channel().StringConfigForKey(configPassphrase, "")
	if passphrase == "" {
		return nil, fmt.Errorf("Missing 'passphrase' config for GL channel")
	}

	status := h.Backend().NewMsgStatusForID(msg.Channel(), msg.ID(), courier.MsgErrored)
	parts := handlers.SplitMsg(handlers.GetTextAndAttachments(msg), maxMsgLength)
	for _, part := range parts {
		glMsg := &mtMsg{}
		glMsg.Address = strings.TrimPrefix(msg.URN().Path(), "+")
		glMsg.Message = part
		glMsg.Passphrase = passphrase
		glMsg.AppID = appID
		glMsg.AppSecret = appSecret

		requestBody := &bytes.Buffer{}
		json.NewEncoder(requestBody).Encode(glMsg)

		// build our request
		req, err := http.NewRequest(http.MethodPost, fmt.Sprintf(sendURL, msg.Channel().Address()), requestBody)
		req.Header.Set("Content-Type", "application/json")
		req.Header.Set("Accept", "application/json")
		if err != nil {
			return nil, err
		}

		rr, err := utils.MakeHTTPRequest(req)
		log := courier.NewChannelLogFromRR("Message Sent", msg.Channel(), msg.ID(), rr).WithError("Message Send Error", err)
		status.AddLog(log)
		if err != nil {
			return status, nil
		}
		status.SetStatus(courier.MsgWired)
	}

	return status, nil
}
