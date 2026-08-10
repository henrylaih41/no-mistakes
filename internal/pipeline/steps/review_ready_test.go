package steps

import (
	"testing"

	"github.com/kunchenguid/no-mistakes/internal/config"
)

// stampReviewReadyIfPassed persists the run's first review-ready moment iff the
// CI monitor's latest state is "checks passed" — green CI or the canonical
// zero-CI state — and no-ops otherwise. It reuses cimonitor's readiness
// decision, so both ready messages stamp while an in-flight message does not.
func TestStampReviewReadyIfPassed(t *testing.T) {
	t.Parallel()
	cases := []struct {
		name       string
		monitorLog string
		wantSet    bool
	}{
		{"green checks passed", ciChecksPassedMsg, true},
		{"zero-CI no checks reported", ciNoChecksPassedMsg, true},
		{"checks still running", ciChecksRunningMsg, false},
	}
	for _, tc := range cases {
		tc := tc
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			sctx := newTestContextWithDBRecords(t, &mockAgent{name: "test"}, t.TempDir(), "base", "head", config.Commands{})

			stampReviewReadyIfPassed(sctx, tc.monitorLog)

			got, err := sctx.DB.GetRun(sctx.Run.ID)
			if err != nil {
				t.Fatalf("get run: %v", err)
			}
			switch {
			case tc.wantSet && got.ReviewReadySince == nil:
				t.Fatalf("ReviewReadySince = nil, want a timestamp for ready monitor log %q", tc.monitorLog)
			case !tc.wantSet && got.ReviewReadySince != nil:
				t.Fatalf("ReviewReadySince = %d, want nil for non-ready monitor log %q", *got.ReviewReadySince, tc.monitorLog)
			}
		})
	}
}
