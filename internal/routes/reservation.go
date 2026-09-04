package routes

import (
	"context"
	"errors"
	"fmt"
	"net/netip"

	"github.com/crusoecloud/crusoe-cloud-controller-manager/internal/routes/sdn"
	"k8s.io/klog/v2"
)

// ErrNoReservationForCIDR indicates no configured reservation contains the
// node's pod cidr (reservation/env drift; needs operator attention).
var ErrNoReservationForCIDR = errors.New("no configured vpc prefix reservation contains pod cidr")

// reservationForCIDR picks the configured reservation containing cidr. With a
// single configured reservation (the pre-expansion norm) it is returned with no
// lookup, SDN validates containment on create anyway. With several (pod-range
// expansion adds reservations; there is no resize), the reservation ranges are
// fetched (once, see cachedReservations) and matched by containment: cilium
// allocated the cidr from exactly one of them.
func (r *CloudRoutes) reservationForCIDR(ctx context.Context, cidr string) (string, error) {
	ids := r.cfg.VPCPrefixReservationIDs
	if len(ids) == 1 {
		return ids[0], nil
	}

	prefix, err := netip.ParsePrefix(cidr)
	if err != nil {
		return "", fmt.Errorf("failed to parse pod cidr %s: %w", cidr, err)
	}

	rsvs, err := r.cachedReservations(ctx)
	if err != nil {
		return "", err
	}
	for i := range rsvs {
		p, perr := netip.ParsePrefix(rsvs[i].Prefix)
		if perr != nil {
			klog.ErrorS(perr, "skipping reservation with unparseable prefix",
				"reservationID", rsvs[i].ID, "prefix", rsvs[i].Prefix)

			continue
		}
		if p.Bits() <= prefix.Bits() && p.Contains(prefix.Addr()) {
			return rsvs[i].ID, nil
		}
	}

	return "", fmt.Errorf("%w: %s (reservations %v)", ErrNoReservationForCIDR, cidr, ids)
}

// cachedReservations returns the configured reservations, fetching them on the
// first call only: reservations are immutable, so one fetch serves every
// create. A failed fetch is not cached.
func (r *CloudRoutes) cachedReservations(ctx context.Context) ([]sdn.VPCPrefixReservation, error) {
	r.mu.Lock()
	cached := r.reservations
	r.mu.Unlock()
	if cached != nil {
		return cached, nil
	}

	rsvs, err := r.sdn.ListVPCPrefixReservations(ctx, r.cfg.VPCPrefixReservationIDs)
	if err != nil {
		return nil, fmt.Errorf("failed to list vpc prefix reservations: %w", err)
	}

	r.mu.Lock()
	r.reservations = rsvs
	r.mu.Unlock()

	return rsvs, nil
}
