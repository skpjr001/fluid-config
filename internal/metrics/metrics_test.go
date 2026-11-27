package metrics

import (
	"strings"
	"testing"
)

func TestHistogramBucketsAreCumulative(t *testing.T) {
	m := NewMetrics()
	for _, v := range []float64{0.0005, 0.003, 0.003, 9} {
		m.Observe("h", v, "api", "a")
	}
	var b strings.Builder
	m.WritePrometheus(&b)
	for _, want := range []string{`h_bucket{api="a",le="0.001"} 1`, `h_bucket{api="a",le="0.005"} 3`,
		`h_bucket{api="a",le="5"} 3`, `h_bucket{api="a",le="+Inf"} 4`, `h_count{api="a"} 4`} {
		if !strings.Contains(b.String(), want) {
			t.Errorf("lack %s in\n%s", want, b.String())
		}
	}
}
