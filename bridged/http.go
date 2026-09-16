package bridged

import (
	"context"
	"encoding/hex"
	"encoding/json"
	"errors"
	"net/http"
	"time"

	"github.com/paulscode/lightning-fork-bridge/node"
	"github.com/paulscode/lightning-fork-bridge/quote"
	"github.com/paulscode/lightning-fork-bridge/store"
)

// QuoteRequest asks for a swap.
type QuoteRequest struct {
	// Invoice is the payment request the user wants paid, on whichever
	// chain it is for. Which direction that makes it is worked out by
	// asking the nodes, not by reading it here.
	Invoice string `json:"invoice"`
}

// QuoteResponse is what the user must do, and what it costs.
type QuoteResponse struct {
	// HoldInvoice is what the user pays, on the other chain. It does not
	// settle until the bridge has delivered, which is the whole mechanism.
	HoldInvoice string `json:"holdInvoice"`

	// Hash is the payment hash both legs share, and the key to ask about
	// this swap later.
	Hash string `json:"hash"`

	// IncomingMsat is what the user owes, OutgoingMsat what the
	// destination receives. They are in different units: the two chains'
	// millisatoshis are not the same thing.
	IncomingMsat uint64 `json:"incomingMsat"`
	OutgoingMsat uint64 `json:"outgoingMsat"`

	// Rate is outgoing units per incoming unit, Spread the fraction
	// charged on top.
	Rate   float64 `json:"rate"`
	Spread float64 `json:"spread"`

	// CLTVDelta is the incoming leg's required CLTV, in blocks of the
	// incoming chain.
	CLTVDelta uint32 `json:"cltvDelta"`

	// Direction names which way this swap runs, for a user who wants to
	// know which node to pay from.
	Direction string `json:"direction"`

	// ExpiresAt is when the quote stops being honoured.
	ExpiresAt time.Time `json:"expiresAt"`
}

// SwapResponse is what the bridge believes about a swap.
type SwapResponse struct {
	Hash         string `json:"hash"`
	State        string `json:"state"`
	IncomingMsat uint64 `json:"incomingMsat"`
	OutgoingMsat uint64 `json:"outgoingMsat"`

	// Preimage is present once there is one, which is the user's proof the
	// destination was paid.
	Preimage string `json:"preimage,omitempty"`
}

// Handler is the daemon's HTTP surface.
//
// Deliberately small. A quote commits the bridge's money, so this belongs on
// loopback or behind something that authenticates; there is no authentication
// here and adding a weak one would be worse than saying so.
func (s *Service) Handler() http.Handler {
	mux := http.NewServeMux()
	mux.HandleFunc("POST /quote", s.handleQuote)
	mux.HandleFunc("GET /swap/{hash}", s.handleSwap)
	mux.HandleFunc("GET /health", s.handleHealth)

	return mux
}

func writeJSON(w http.ResponseWriter, code int, v any) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(code)
	_ = json.NewEncoder(w).Encode(v)
}

func writeErr(w http.ResponseWriter, code int, err error) {
	writeJSON(w, code, map[string]string{"error": err.Error()})
}

func (s *Service) handleQuote(w http.ResponseWriter, r *http.Request) {
	var req QuoteRequest
	dec := json.NewDecoder(http.MaxBytesReader(w, r.Body, 1<<16))
	dec.DisallowUnknownFields()
	if err := dec.Decode(&req); err != nil {
		writeErr(w, http.StatusBadRequest, err)

		return
	}
	if req.Invoice == "" {
		writeErr(w, http.StatusBadRequest, errors.New("no invoice"))

		return
	}

	ctx := r.Context()
	sd, _, err := s.route(ctx, req.Invoice)
	if err != nil {
		writeErr(w, http.StatusBadRequest, err)

		return
	}

	terms, err := sd.quoter.Quote(ctx, req.Invoice)
	if err != nil {
		// A refusal is the bridge working, not failing: too large, too
		// close to expiry, not enough headroom. It is the user's to act
		// on, so it is a 4xx with the reason intact.
		code := http.StatusBadGateway
		if errors.Is(err, quote.ErrRefused) {
			code = http.StatusConflict
		}
		writeErr(w, code, err)

		return
	}

	// The swap is now the bridge's to finish: the hold invoice is live and
	// the user may pay it at any moment. Drive it on a context that
	// outlives this request, or a user who disconnects after paying would
	// leave an HTLC nobody is watching.
	s.drive(sd, terms.Hash)

	writeJSON(w, http.StatusOK, QuoteResponse{
		HoldInvoice: terms.HoldInvoice, Hash: hex.EncodeToString(terms.Hash[:]),
		IncomingMsat: terms.IncomingMsat, OutgoingMsat: terms.OutgoingMsat,
		Rate: terms.Rate, Spread: terms.Spread, CLTVDelta: terms.CLTVDelta,
		Direction: sd.name, ExpiresAt: terms.Expires,
	})
}

func (s *Service) handleSwap(w http.ResponseWriter, r *http.Request) {
	raw, err := hex.DecodeString(r.PathValue("hash"))
	if err != nil || len(raw) != len(node.Hash{}) {
		writeErr(w, http.StatusBadRequest,
			errors.New("the hash must be 32 hex-encoded bytes"))

		return
	}
	var hash node.Hash
	copy(hash[:], raw)

	rec, err := s.journal.Get(r.Context(), hash)
	if err != nil {
		if errors.Is(err, store.ErrNotFound) {
			writeErr(w, http.StatusNotFound, err)

			return
		}
		writeErr(w, http.StatusInternalServerError, err)

		return
	}

	resp := SwapResponse{
		Hash: hex.EncodeToString(rec.Hash[:]), State: rec.State.String(),
		IncomingMsat: rec.IncomingMsat, OutgoingMsat: rec.OutgoingMsat,
	}
	if rec.Preimage != (node.Preimage{}) {
		resp.Preimage = hex.EncodeToString(rec.Preimage[:])
	}
	writeJSON(w, http.StatusOK, resp)
}

// handleHealth answers whether the daemon could quote right now, which is a
// different question from whether it is running. A bridge with no price or a
// chain it cannot see is up and unable to do anything, and saying "ok" to
// that is how it goes unnoticed.
func (s *Service) handleHealth(w http.ResponseWriter, r *http.Request) {
	ctx := r.Context()
	out := map[string]any{"directions": len(s.sides)}

	s.priceMu.RLock()
	priceErr := s.priceErr
	price := s.price
	s.priceMu.RUnlock()

	ready := true
	if priceErr != nil {
		out["price"] = priceErr.Error()
		ready = false
	} else {
		out["price"] = price.OutgoingPerIncoming
		out["priceSources"] = price.Sources
	}

	for _, name := range []string{"blake2b", "bitcoin"} {
		obs := s.lfChain
		if name == "bitcoin" {
			obs = s.btcChain
		}
		if _, err := obs.Estimate(time.Now()); err != nil {
			out[name+"Chain"] = err.Error()
			ready = false
		} else {
			out[name+"Chain"] = "ok"
		}
	}

	// Being unable to reach a node is the other way this is up and useless.
	for _, sd := range s.sides {
		if _, err := s.outgoingBalance(ctx, sd); err != nil {
			out[sd.name] = err.Error()
			ready = false
		} else {
			out[sd.name] = "ok"
		}
	}

	out["ready"] = ready
	code := http.StatusOK
	if !ready {
		code = http.StatusServiceUnavailable
	}
	writeJSON(w, code, out)
}

// drive finishes a swap in the background, on the service's own context.
func (s *Service) drive(sd *side, hash node.Hash) {
	s.wg.Add(1)
	go func() {
		defer s.wg.Done()

		if err := sd.runner.Drive(s.ctx, hash); err != nil {
			s.log.Error("swap did not finish", "direction", sd.name,
				"hash", hex.EncodeToString(hash[:]), "err", err)
		}
	}()
}

// ctxOrBackground is used where a caller has not started the service.
func ctxOrBackground(ctx context.Context) context.Context {
	if ctx == nil {
		return context.Background()
	}

	return ctx
}
