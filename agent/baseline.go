package agent

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"regexp"
	"strings"
	"time"
)

// Baseline is the bar an agent has to beat: the keyword rules plus status
// template that a help desk would run without a model. It uses the same
// tools and guards as the agent, answers order-status questions, and
// escalates everything else.
func Baseline(ctx context.Context, api *OrdersAPI, t Ticket) (Decision, error) {
	start := time.Now()
	tb := &toolbox{api: api, sender: t.From}
	text := strings.ToLower(t.Subject + "\n" + t.Body)
	id := orderNumber.FindString(strings.ToUpper(text))
	escalate := func(reason string) (Decision, error) {
		return tb.finish(Decision{Action: ActionEscalate, OrderID: id, Reply: holdingReply, EscalationReason: reason}, start, ""), nil
	}

	for _, w := range escalationWords {
		if strings.Contains(text, w) {
			return escalate("keyword: " + w)
		}
	}

	if id == "" {
		list, err := tb.listOrders(ctx)
		if err != nil {
			return escalate("no account for sender")
		}
		orders := list.([]order)
		if len(orders) == 0 {
			return escalate("no orders on account")
		}
		id = orders[0].ID // newest
	}

	o, err := tb.owned(ctx, id)
	if errors.Is(err, errNotYours) {
		return tb.finish(Decision{Action: ActionReply, Reply: "We can only discuss an order with its account holder. " +
			"Please write to us from the email address on the order.\n\nExample Print Co. Support"}, start, ""), nil
	}
	if err != nil {
		return Decision{}, err
	}

	var ship struct {
		Carrier        string `json:"carrier"`
		TrackingNumber string `json:"trackingNumber"`
		Status         string `json:"status"`
	}
	_ = json.Unmarshal(o.Shipment, &ship)
	var reply string
	switch {
	case ship.Status == "exception":
		return escalate("shipment exception")
	case o.Status == "shipped" || o.Status == "delivered":
		reply = fmt.Sprintf("Your order %s is %s. Carrier: %s, tracking number %s.", o.ID, o.Status, ship.Carrier, ship.TrackingNumber)
	case o.Status == "in_production" && o.ShipBy != nil:
		reply = fmt.Sprintf("Your order %s is in production and will ship by %s.", o.ID, *o.ShipBy)
	case o.Status == "proof_pending":
		reply = fmt.Sprintf("Your order %s is waiting for you to approve the proof in your account. Production starts once you approve it.", o.ID)
	case o.Status == "cancelled":
		reply = fmt.Sprintf("Your order %s was cancelled.", o.ID)
	default:
		return escalate("unknown status " + o.Status)
	}
	return tb.finish(Decision{Action: ActionReply, OrderID: o.ID, Reply: reply + "\n\nExample Print Co. Support"}, start, ""), nil
}

var orderNumber = regexp.MustCompile(`ORD-\d+`)

var escalationWords = []string{
	"refund", "cancel", "chargeback", "lawyer", "manager", "charged", "invoice", "discount", "quote",
	"damaged", "broken", "cracked", "crushed", "misprint", "wrong", "defect", "peeling", "address",
}
