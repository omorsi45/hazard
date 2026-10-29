package control

// DriftStatus is the drift monitor's verdict: State is "warming", "ok" or
// "alarm". The full definition arrives with the monitor.
type DriftStatus struct {
	State  string
	N      int
	Misses int
	UCB    float64
	Alarm  bool
}
