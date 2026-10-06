package httpx

import (
	"errors"
	"net/http"
	"strconv"
	"time"

	"github.com/go-chi/chi/v5"

	"github.com/jking323/ws/internal/fleet/rental"
	"github.com/jking323/ws/internal/store"
)

// rentalView is an instance plus derived figures for the page.
type rentalView struct {
	store.RentalInstance
	// EstimatedUSD is hours_used times the hourly price so far.
	EstimatedUSD float64 `json:"estimated_usd"`
}

func rentalViewOf(r store.RentalInstance) rentalView {
	return rentalView{RentalInstance: r, EstimatedUSD: float64(r.HoursUsed) * r.HourlyUsd}
}

// handleListRentals returns the templates, the instances and the caps.
func (s *Server) handleListRentals(w http.ResponseWriter, r *http.Request) {
	if s.Rental == nil {
		writeErr(w, 503, "rentals are not configured")
		return
	}
	limit, _ := strconv.Atoi(r.URL.Query().Get("limit"))
	if limit <= 0 || limit > 200 {
		limit = 50
	}
	rows, err := s.DB.ListRentalInstances(r.Context(), int32(limit))
	if err != nil {
		writeErr(w, 500, "db")
		return
	}
	out := make([]rentalView, 0, len(rows))
	for _, x := range rows {
		out = append(out, rentalViewOf(x))
	}
	now := time.Now().UTC()
	day := time.Date(now.Year(), now.Month(), now.Day(), 0, 0, 0, 0, time.UTC)
	used, _ := s.DB.SumRentalHoursSince(r.Context(), day)
	providers := map[string]bool{}
	for name := range s.Rental.Providers {
		providers[name] = true
	}
	tpls := s.Rental.Templates
	if tpls == nil {
		tpls = []rental.Template{}
	}
	writeJSON(w, 200, map[string]any{
		"templates": tpls, "instances": out, "providers": providers,
		"caps": map[string]any{"daily_cap_hours": s.Rental.DailyCapHours, "used_today_hours": used, "disabled": s.Rental.Disabled, "key_set": s.Rental.Getenv == nil || s.Rental.Getenv("WS_RENTAL_API_KEY") != ""},
	})
}

func (s *Server) handleStartRental(w http.ResponseWriter, r *http.Request) {
	if s.Rental == nil {
		writeErr(w, 503, "rentals are not configured")
		return
	}
	var in struct {
		Template string `json:"template"`
	}
	if err := decode(r, &in); err != nil || in.Template == "" {
		writeErr(w, 400, "template required")
		return
	}
	row, err := s.Rental.Start(r.Context(), in.Template, Principal(r.Context()).UserID)
	if err != nil {
		writeRentalErr(w, err)
		return
	}
	writeJSON(w, 201, rentalViewOf(*row))
}

func (s *Server) handleStopRental(w http.ResponseWriter, r *http.Request) {
	if s.Rental == nil {
		writeErr(w, 503, "rentals are not configured")
		return
	}
	id, err := uuidParam(r, "id")
	if err != nil {
		writeErr(w, 400, "bad id")
		return
	}
	if err := s.Rental.Stop(r.Context(), id, "stopped from Admin"); err != nil {
		writeRentalErr(w, err)
		return
	}
	row, _ := s.DB.GetRentalInstance(r.Context(), id)
	writeJSON(w, 200, rentalViewOf(row))
}

func (s *Server) handleStopAllRentals(w http.ResponseWriter, r *http.Request) {
	if s.Rental == nil {
		writeErr(w, 503, "rentals are not configured")
		return
	}
	if err := s.Rental.StopAll(r.Context(), "stop all from Admin"); err != nil {
		writeRentalErr(w, err)
		return
	}
	writeJSON(w, 200, map[string]string{"status": "ok"})
}

func (s *Server) handleRentalOffers(w http.ResponseWriter, r *http.Request) {
	if s.Rental == nil {
		writeErr(w, 503, "rentals are not configured")
		return
	}
	offers, err := s.Rental.Offers(r.Context(), chi.URLParam(r, "provider"))
	if err != nil {
		writeRentalErr(w, err)
		return
	}
	writeJSON(w, 200, map[string]any{"offers": offers})
}

func writeRentalErr(w http.ResponseWriter, err error) {
	switch {
	case errors.Is(err, rental.ErrNoTemplate):
		writeErr(w, 404, "unknown template")
	case errors.Is(err, rental.ErrBusy):
		writeErr(w, 409, "this template already has a machine")
	case errors.Is(err, rental.ErrDisabled), errors.Is(err, rental.ErrDailyCap), errors.Is(err, rental.ErrNoAPIKey), errors.Is(err, rental.ErrNoProvider):
		writeErr(w, 409, err.Error())
	case errors.Is(err, rental.ErrNotAvailable):
		writeErr(w, 503, err.Error())
	default:
		writeErr(w, 502, err.Error())
	}
}
