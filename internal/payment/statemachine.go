// Package payment implements the payment-intent lifecycle: creation,
// tokenization, fraud screening, authorization (with 3-D Secure and manual
// review detours), capture, void, cancellation, refunds and authorization
// expiry, all guarded by an explicit state machine.
package payment

import (
	"github.com/robertramos/enterprise-payment-gateway/internal/apperr"
	"github.com/robertramos/enterprise-payment-gateway/internal/model"
)

// Action is an operation applied to a payment intent.
type Action string

// Actions.
const (
	ActAuthorize     Action = "authorize"
	ActComplete3DS   Action = "complete_3ds"
	ActReviewApprove Action = "review_approve"
	ActReviewReject  Action = "review_reject"
	ActCapture       Action = "capture"
	ActVoid          Action = "void"
	ActExpire        Action = "expire"
	ActCancel        Action = "cancel"
	ActRefund        Action = "refund"
)

type rule struct {
	from []model.PaymentStatus
	to   []model.PaymentStatus
}

// transitions is the complete state machine. Any (status, action, target)
// combination not listed here is rejected.
var transitions = map[Action]rule{
	ActAuthorize: {
		from: []model.PaymentStatus{model.StatusCreated, model.StatusDeclined},
		to: []model.PaymentStatus{model.StatusAuthorized, model.StatusRequiresAction, model.StatusRequiresReview,
			model.StatusDeclined, model.StatusBlocked},
	},
	ActComplete3DS: {
		from: []model.PaymentStatus{model.StatusRequiresAction},
		to:   []model.PaymentStatus{model.StatusAuthorized, model.StatusDeclined},
	},
	ActReviewApprove: {
		from: []model.PaymentStatus{model.StatusRequiresReview},
		to:   []model.PaymentStatus{model.StatusAuthorized, model.StatusRequiresAction, model.StatusDeclined},
	},
	ActReviewReject: {
		from: []model.PaymentStatus{model.StatusRequiresReview},
		to:   []model.PaymentStatus{model.StatusBlocked},
	},
	ActCapture: {
		from: []model.PaymentStatus{model.StatusAuthorized},
		to:   []model.PaymentStatus{model.StatusCaptured},
	},
	ActVoid: {
		from: []model.PaymentStatus{model.StatusAuthorized},
		to:   []model.PaymentStatus{model.StatusVoided},
	},
	ActExpire: {
		from: []model.PaymentStatus{model.StatusAuthorized},
		to:   []model.PaymentStatus{model.StatusVoided},
	},
	ActCancel: {
		from: []model.PaymentStatus{model.StatusCreated, model.StatusRequiresAction, model.StatusRequiresReview,
			model.StatusDeclined},
		to: []model.PaymentStatus{model.StatusCanceled},
	},
	ActRefund: {
		from: []model.PaymentStatus{model.StatusCaptured, model.StatusPartiallyRefunded},
		to:   []model.PaymentStatus{model.StatusPartiallyRefunded, model.StatusRefunded},
	},
}

func contains(list []model.PaymentStatus, s model.PaymentStatus) bool {
	for _, x := range list {
		if x == s {
			return true
		}
	}
	return false
}

// CanApply reports whether action may be applied in status from.
func CanApply(from model.PaymentStatus, a Action) bool {
	r, ok := transitions[a]
	return ok && contains(r.from, from)
}

// CheckApply returns an invalid-state error if the action is not allowed.
func CheckApply(from model.PaymentStatus, a Action) error {
	if !CanApply(from, a) {
		return apperr.InvalidState("payment intent", string(from), string(a))
	}
	return nil
}

// CheckTransition validates a full (from, action, to) move.
func CheckTransition(from model.PaymentStatus, a Action, to model.PaymentStatus) error {
	if err := CheckApply(from, a); err != nil {
		return err
	}
	if !contains(transitions[a].to, to) {
		return apperr.New(apperr.CodeInvalidState, "action %s cannot move a payment intent to %q", a, to)
	}
	return nil
}

// Terminal reports whether no further action can change the status.
func Terminal(s model.PaymentStatus) bool {
	for _, r := range transitions {
		if contains(r.from, s) {
			return false
		}
	}
	return true
}

// AllowedActions lists the actions permitted in a status.
func AllowedActions(s model.PaymentStatus) []Action {
	var out []Action
	for _, a := range []Action{ActAuthorize, ActComplete3DS, ActReviewApprove, ActReviewReject, ActCapture, ActVoid,
		ActExpire, ActCancel, ActRefund} {
		if CanApply(s, a) {
			out = append(out, a)
		}
	}
	return out
}
