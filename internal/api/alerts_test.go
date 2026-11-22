package api

import "testing"

func TestCadence(t *testing.T) {
	day := int64(86400)
	mk := func(step int64, n int) []int64 {
		var d []int64
		for i := 0; i < n; i++ {
			d = append(d, 1_000_000_000-int64(i)*step*day)
		}
		return d
	}
	for _, c := range []struct {
		dates []int64
		want  float64
	}{
		{mk(7, 10), 7}, {mk(14, 6), 15}, {mk(30, 5), 30}, {mk(90, 6), 0}, {mk(7, 3), 0},
		{[]int64{100 * day, 93 * day, 86 * day, 10 * day, 3 * day}, 7}, // un parón largo no cambia el ritmo
	} {
		if got := cadence(c.dates); got != c.want {
			t.Errorf("cadence(%v) = %v, quiero %v", c.dates, got, c.want)
		}
	}
}
