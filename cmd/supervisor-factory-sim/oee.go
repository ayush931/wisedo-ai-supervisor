package main

import (
	"context"
	"encoding/json"
	"fmt"
	"github.com/Wisedo-Technology/wisedo-ai-supervisor/internal/factorysim"
	"github.com/Wisedo-Technology/wisedo-ai-supervisor/internal/oee"
	"time"
)

func factoryPolicy() oee.OeePolicyBodyV1 {
	var p oee.OeePolicyBodyV1
	// Same governed policy shape as the OEE service fixture; 60s separates microstops.
	if e := json.Unmarshal([]byte(`{"policy_version":"1.0.0","precedence_order":["SHUTDOWN_EXCLUSION","PLANNED_BREAK","CHANGEOVER","UNPLANNED_DOWNTIME","STANDBY","RUNNING"],"min_coverage":"1","max_source_silence_seconds":0,"rounding_scale":6,"rounding_mode":"HALF_EVEN","zero_denominator_rule":"NOT_READY","performance_cap_rule":"ALLOW_AND_RETAIN","rework_counts_as_good":false,"microstop_threshold_seconds":"60","state_buckets":[{"state":"RUNNING","bucket":"RUN"},{"state":"UNPLANNED_DOWNTIME","bucket":"STOP"},{"state":"CHANGEOVER","bucket":"STOP"},{"state":"STANDBY","bucket":"STOP"},{"state":"PLANNED_BREAK","bucket":"EXCLUDED"},{"state":"SHUTDOWN_EXCLUSION","bucket":"EXCLUDED"}]}`), &p); e != nil {
		panic(e)
	}
	return p
}
func (l *loader) loadOEE(ctx context.Context, d dataset) (int, error) {
	path := "/oee/policies/" + d.id("oee-policy")
	var policy oee.OeePolicyV1
	if e := l.call(ctx, "owner", path+"/revisions", "oee-policy", "", json.RawMessage(d.files["oee-policy.json"]), &policy); e != nil {
		return 0, e
	}
	if len(policy.Revisions) != 1 {
		return 0, fmt.Errorf("synthetic OEE policy changed externally")
	}
	if _, e := l.request(ctx, "approver", "POST", "/v1/sites/"+factorysim.SiteID+path+"/revisions/"+policy.Revisions[0].PolicyRevisionID+":publish", "oee-publish", "1", "application/json", nil, nil); e != nil {
		return 0, e
	}
	count := 0
	for _, f := range d.manifest.Files {
		if f.Schema != "OeeCalculationRequestV1" {
			continue
		}
		var request oee.OeeCalculationRequestV1
		if e := json.Unmarshal(l.bind(d.files[f.Path]), &request); e != nil {
			return count, e
		}
		var e error
		request.Dataset.ProvenanceDigest, e = oee.DatasetProvenanceDigest(request.Dataset)
		if e != nil {
			return count, e
		}
		var run oee.OeeCalculationRunV1
		if e = l.call(ctx, "owner", "/oee/calculations", f.Path, "", request, &run); e != nil {
			return count, e
		}
		timeout := time.NewTimer(30 * time.Second)
		ticker := time.NewTicker(25 * time.Millisecond)
		for run.Status == oee.RunAccepted || run.Status == oee.RunRunning {
			select {
			case <-ctx.Done():
				e = ctx.Err()
			case <-timeout.C:
				e = fmt.Errorf("OEE calculation timed out")
			case <-ticker.C:
				e = l.call(ctx, "owner", "/oee/calculations/"+run.RunID, "", "", nil, &run)
			}
			if e != nil {
				break
			}
		}
		timeout.Stop()
		ticker.Stop()
		if e != nil {
			return count, e
		}
		if run.Status != oee.RunSucceeded || run.ResultStatus != oee.StatusReady {
			return count, fmt.Errorf("%s: OEE %s %s %s", f.Path, run.Status, run.ResultStatus, run.FailureCode)
		}
		count++
	}
	return count, nil
}
