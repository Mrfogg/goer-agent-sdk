package base

import "testing"

func TestContextCompactionThresholdTokens(t *testing.T) {
	// The threshold is contextCompactionThresholdRatio of the window, and the window is
	// the only input.
	cases := []struct {
		name   string
		window int
		want   int
	}{
		{name: "default window", window: 0, want: 600_000},
		{name: "explicit 1M window", window: 1_000_000, want: 600_000},
		{name: "50k window uses ratio", window: 50_000, want: 30_000},
		{name: "100k window uses ratio", window: 100_000, want: 60_000},
		{name: "128k window", window: 128_000, want: 76_800},
		{name: "4M window", window: 4_000_000, want: 2_400_000},
		// An invalid window falls back to the default.
		{name: "negative window falls back", window: -1, want: 600_000},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			agent := &BaseAgent{maxContextTokens: tc.window}
			if got := agent.contextCompactionThresholdTokens(); got != tc.want {
				t.Fatalf("contextCompactionThresholdTokens() = %d, want %d", got, tc.want)
			}
		})
	}
}

func TestShouldCompressContext(t *testing.T) {
	agent := &BaseAgent{maxContextTokens: 100_000} // threshold 60k
	threshold := agent.contextCompactionThresholdTokens()
	if threshold != 60_000 {
		t.Fatalf("precondition: threshold = %d, want 60000", threshold)
	}

	if agent.shouldCompressContext(threshold) {
		t.Fatalf("shouldCompressContext(threshold) = true, want false (strictly greater triggers)")
	}
	if !agent.shouldCompressContext(threshold + 1) {
		t.Fatalf("shouldCompressContext(threshold+1) = false, want true")
	}
	// No usable usage never triggers.
	if agent.shouldCompressContext(0) {
		t.Fatalf("shouldCompressContext(0) = true, want false")
	}
	if agent.shouldCompressContext(-1) {
		t.Fatalf("shouldCompressContext(-1) = true, want false")
	}
}

func TestContextCompactionKeepTokens(t *testing.T) {
	// keep = contextCompactionKeepRatio of the threshold; the default 1M window →
	// threshold 600k → keep 150k.
	agent := &BaseAgent{}
	if got, want := agent.contextCompactionKeepTokens(), 150_000; got != want {
		t.Fatalf("contextCompactionKeepTokens() = %d, want %d", got, want)
	}

	// A small window: threshold 60k → keep 15k.
	small := &BaseAgent{maxContextTokens: 100_000}
	if got, want := small.contextCompactionKeepTokens(), 15_000; got != want {
		t.Fatalf("contextCompactionKeepTokens(small) = %d, want %d", got, want)
	}
}

func TestRecordPromptTokens(t *testing.T) {
	agent := &BaseAgent{}
	agent.recordPromptTokens(1234)
	if agent.lastPromptTokens != 1234 {
		t.Fatalf("lastPromptTokens = %d, want 1234", agent.lastPromptTokens)
	}

	// Missing (0) or invalid usage must not overwrite the known value, otherwise the
	// trigger state would be cleared by accident.
	agent.recordPromptTokens(0)
	agent.recordPromptTokens(-5)
	if agent.lastPromptTokens != 1234 {
		t.Fatalf("lastPromptTokens = %d after zero/negative record, want 1234", agent.lastPromptTokens)
	}

	// A new value refreshes normally.
	agent.recordPromptTokens(2345)
	if agent.lastPromptTokens != 2345 {
		t.Fatalf("lastPromptTokens = %d, want 2345", agent.lastPromptTokens)
	}
}

func TestWithMaxContextTokens_IgnoresNonPositive(t *testing.T) {
	agent := &BaseAgent{maxContextTokens: defaultMaxContextTokens}
	agent.WithMaxContextTokens(0)
	agent.WithMaxContextTokens(-10)
	if agent.maxContextTokens != defaultMaxContextTokens {
		t.Fatalf("maxContextTokens = %d, want the default %d", agent.maxContextTokens, defaultMaxContextTokens)
	}
	agent.WithMaxContextTokens(64_000)
	if agent.maxContextTokens != 64_000 {
		t.Fatalf("maxContextTokens = %d, want 64000", agent.maxContextTokens)
	}
}
