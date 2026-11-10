package bench

import (
	"cmp"
	"encoding/json"
	"math"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/josh10111/coldprobe/internal/runpod"
)

// validParams is the smallest Params that passes Validate.
func validParams() Params {
	p := DefaultParams("ghcr.io/josh10111/coldprobe-worker:latest")
	p.Pools = []string{"AMPERE_16"}
	return p
}

func TestDefaultParams(t *testing.T) {
	p := DefaultParams("img")
	if p.Image != "img" || p.Pools != nil || len(p.FlashBoot) != 2 || p.FlashBoot[0] != "OFF" || p.FlashBoot[1] != "FLASHBOOT" ||
		p.MaxWorkers != 3 || p.IdleTimeoutS != 5 || p.ColdStarts != 3 || p.WarmRequests != 10 ||
		len(p.LevelsRPS) != 3 || p.LevelsRPS[0] != 1 || p.LevelsRPS[1] != 3 || p.LevelsRPS[2] != 8 ||
		p.LevelSeconds != 30 || p.DrainSeconds != 60 || p.JobSleepMs != 500 || p.ModelLoadS != 5 || p.BudgetUSD != 2.00 {
		t.Errorf("DefaultParams = %+v", p)
	}
}

func TestValidate(t *testing.T) {
	tests := []struct {
		name   string
		modify func(p *Params)
		capUSD float64
		field  string // text the error must contain (the JSON name, or more); "" means no error
	}{
		{"defaults plus a pool", func(p *Params) {}, 2, ""},
		{"two pools", func(p *Params) { p.Pools = []string{"AMPERE_16", "AMPERE_24"} }, 2, ""},
		{"budget equal to cap", func(p *Params) { p.BudgetUSD = 2 }, 2, ""},
		{"maxWorkers 60 (the default limit)", func(p *Params) { p.MaxWorkers = 60 }, 2, ""},
		{"maxWorkers 61", func(p *Params) { p.MaxWorkers = 61 }, 2, "maxWorkers: 61 must be between 1 and 60"},
		{"maxWorkers 0", func(p *Params) { p.MaxWorkers = 0 }, 2, "maxWorkers"},
		{"budget 0", func(p *Params) { p.BudgetUSD = 0 }, 2, "budgetUsd"},
		{"budget above cap", func(p *Params) { p.BudgetUSD = 2.01 }, 2, "budgetUsd"},
		{"budget NaN", func(p *Params) { p.BudgetUSD = math.NaN() }, 2, "budgetUsd"},
		{"0 pools", func(p *Params) { p.Pools = nil }, 2, "pools"},
		{"3 pools", func(p *Params) { p.Pools = []string{"AMPERE_16", "AMPERE_24", "ADA_24"} }, 2, "pools"},
		{"flashboot ON", func(p *Params) { p.FlashBoot = []string{"ON"} }, 2, "flashboot"},
		{"flashboot empty", func(p *Params) { p.FlashBoot = nil }, 2, "flashboot"},
		{"duplicate pool", func(p *Params) { p.Pools = []string{"AMPERE_16", "AMPERE_16"} }, 2, "pools: duplicate AMPERE_16"},
		{"duplicate flashboot", func(p *Params) { p.FlashBoot = []string{"OFF", "OFF"} }, 2, "flashboot: duplicate OFF"},
		{"4 levels", func(p *Params) { p.LevelsRPS = []float64{1, 2, 4, 8} }, 2, "levelsRps"},
		{"0 levels", func(p *Params) { p.LevelsRPS = nil }, 2, "levelsRps"},
		{"rps 0", func(p *Params) { p.LevelsRPS = []float64{1, 0} }, 2, "levelsRps"},
		{"rps 21", func(p *Params) { p.LevelsRPS = []float64{21} }, 2, "levelsRps"},
		{"duplicate level", func(p *Params) { p.LevelsRPS = []float64{3, 1, 3} }, 2, "levelsRps: duplicate 3"},
		// rps x min(levelSeconds, 60) must stay <= 400.
		{"14 rps x 30 s = 420 open", func(p *Params) { p.LevelsRPS = []float64{14} }, 2, "levelsRps: 14 req/s"},
		{"20 rps x 300 s caps at 60 s = 1200", func(p *Params) { p.LevelsRPS = []float64{20}; p.LevelSeconds = 300 }, 2, "400"},
		{"20 rps x 20 s = 400 open", func(p *Params) { p.LevelsRPS = []float64{20}; p.LevelSeconds = 20 }, 2, ""},
		{"6 rps x 300 s caps at 60 s = 360", func(p *Params) { p.LevelsRPS = []float64{6}; p.LevelSeconds = 300 }, 2, ""},
		{"idle 0", func(p *Params) { p.IdleTimeoutS = 0 }, 2, "idleTimeoutS"},
		{"idle 3601", func(p *Params) { p.IdleTimeoutS = 3601 }, 2, "idleTimeoutS"},
		{"empty image", func(p *Params) { p.Image = " " }, 2, "image"},
		{"cold 11", func(p *Params) { p.ColdStarts = 11 }, 2, "coldStarts"},
		{"warm 51", func(p *Params) { p.WarmRequests = 51 }, 2, "warmRequests"},
		{"levelSeconds 0", func(p *Params) { p.LevelSeconds = 0 }, 2, "levelSeconds"},
		{"drainSeconds -1", func(p *Params) { p.DrainSeconds = -1 }, 2, "drainSeconds"},
		{"jobSleepMs 60001", func(p *Params) { p.JobSleepMs = 60001 }, 2, "jobSleepMs"},
		{"modelLoadS -1", func(p *Params) { p.ModelLoadS = -1 }, 2, "modelLoadS"},
		{"coldStartMode forced", func(p *Params) { p.ColdStartMode = ColdForced }, 2, ""},
		// Empty means natural.
		{"coldStartMode empty", func(p *Params) { p.ColdStartMode = "" }, 2, ""},
		{"coldStartMode bogus", func(p *Params) { p.ColdStartMode = "always" }, 2, "coldStartMode"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			p := validParams()
			tt.modify(&p)
			err := p.Validate(tt.capUSD, DefaultMaxWorkersLimit, fakeCatalog())
			if tt.field == "" {
				if err != nil {
					t.Fatalf("Validate() = %v, want nil", err)
				}
				return
			}
			if err == nil {
				t.Fatalf("Validate() = nil, want an error naming %q", tt.field)
			}
			if !strings.Contains(err.Error(), tt.field) {
				t.Errorf("Validate() = %q, want it to name %q", err, tt.field)
			}
		})
	}
}

// The maxWorkers limit is the value Validate is given, not a constant.
func TestValidate_MaxWorkersLimitIsAParameter(t *testing.T) {
	p := validParams()
	for _, tc := range []struct {
		limit, workers int
		want           string // "" means valid
	}{
		{1, 1, ""},
		{1, 2, "maxWorkers: 2 must be between 1 and 1"},
		{5, 5, ""},
		{5, 6, "maxWorkers: 6 must be between 1 and 5"},
		{60, 60, ""},
		{60, 61, "maxWorkers: 61 must be between 1 and 60"},
	} {
		p.MaxWorkers = tc.workers
		err := p.Validate(2, tc.limit, fakeCatalog())
		if (tc.want == "" && err != nil) || (tc.want != "" && (err == nil || !strings.Contains(err.Error(), tc.want))) {
			t.Errorf("limit %d, maxWorkers %d: Validate() = %v, want %q", tc.limit, tc.workers, err, tc.want)
		}
	}
}

// Both ends of every range pass and one past either end fails.
func TestParamLimits_MatchValidate(t *testing.T) {
	const limit = 4
	lim := ParamLimits(limit)
	setters := map[string]func(p *Params, v int){
		"maxWorkers":   func(p *Params, v int) { p.MaxWorkers = v },
		"idleTimeoutS": func(p *Params, v int) { p.IdleTimeoutS = v },
		"coldStarts":   func(p *Params, v int) { p.ColdStarts = v },
		"warmRequests": func(p *Params, v int) { p.WarmRequests = v },
		"levelSeconds": func(p *Params, v int) {
			p.LevelSeconds = v
			p.LevelsRPS = []float64{1} // so no level hits Runpod's 400 open requests
		},
		"drainSeconds": func(p *Params, v int) { p.DrainSeconds = v },
		"jobSleepMs":   func(p *Params, v int) { p.JobSleepMs = v },
		"modelLoadS":   func(p *Params, v int) { p.ModelLoadS = v },
		"matmulN": func(p *Params, v int) {
			p.MatmulN = v
			p.Image = GPUImage
		},
		"pools": func(p *Params, v int) {
			p.Pools = []string{"AMPERE_16", "AMPERE_24", "ADA_24"}[:min(v, 3)]
		},
		"levelsRps": func(p *Params, v int) { p.LevelsRPS = []float64{1, 2, 3, 4}[:v] },
		// A level's rate: lo itself is refused (above lo), hi passes.
		"rps": func(p *Params, v int) {
			p.LevelsRPS = []float64{float64(v)}
			p.LevelSeconds = 20 // so 20 req/s keeps 400 open, Runpod's limit
		},
	}
	if len(setters) != len(lim) {
		t.Fatalf("ParamLimits has %d keys, the test covers %d: %v", len(lim), len(setters), lim)
	}
	if lim["maxWorkers"] != [2]int{1, limit} {
		t.Errorf("maxWorkers range = %v, want [1 %d]", lim["maxWorkers"], limit)
	}
	field := map[string]string{"pools": "pools", "levelsRps": "levelsRps", "rps": "levelsRps"}
	check := func(key string, v int, ok bool) {
		t.Helper()
		p := validParams()
		setters[key](&p, v)
		err := p.Validate(2, limit, fakeCatalog())
		name := cmp.Or(field[key], key)
		if ok && err != nil {
			t.Errorf("%s = %d: Validate() = %v, want nil", key, v, err)
		}
		if !ok && (err == nil || !strings.Contains(err.Error(), name+":")) {
			t.Errorf("%s = %d: Validate() = %v, want an error naming %s", key, v, err, name)
		}
	}
	for key, r := range lim {
		if _, ok := setters[key]; !ok {
			t.Errorf("ParamLimits key %q has no test setter", key)
			continue
		}
		if key == "rps" {
			check(key, r[0], false)
		} else {
			check(key, r[0], true)
			check(key, r[0]-1, false)
		}
		check(key, r[1], true)
		check(key, r[1]+1, false)
	}
}

func TestValidate_ReportsEveryProblem(t *testing.T) {
	p := validParams()
	p.MaxWorkers = 61
	p.Image = ""
	err := p.Validate(2, DefaultMaxWorkersLimit, fakeCatalog())
	if err == nil || !strings.Contains(err.Error(), "maxWorkers") || !strings.Contains(err.Error(), "image") {
		t.Errorf("Validate() = %v, want both maxWorkers and image", err)
	}
}

func TestDefaultTimeouts(t *testing.T) {
	want := Timeouts{FirstBoot: 600 * time.Second, ScaleDown: 180 * time.Second, Cold: 300 * time.Second,
		Warm: 60 * time.Second, Teardown: 30 * time.Second, NoWorkerWarn: 90 * time.Second}
	if got := DefaultTimeouts(); got != want {
		t.Errorf("DefaultTimeouts() = %+v, want %+v", got, want)
	}
}

func TestConfigDeadline(t *testing.T) {
	// 600 + 3*(180+300) + 10*60 + 3*(30+60) = 2910 s, worked out by hand.
	got := DefaultTimeouts().ConfigDeadline(DefaultParams("img"))
	if got != 2910*time.Second {
		t.Errorf("ConfigDeadline = %v, want 2910s", got)
	}
}

func TestEstimate(t *testing.T) {
	p := validParams()
	p.Pools = []string{"AMPERE_16"}
	p.MaxWorkers = 2 // the figures below were worked by hand with 2
	prices := map[string]float64{"AMPERE_16": 0.72}

	est, err := EstimateRun(p, DefaultTimeouts(), poolsAt(prices))
	if err != nil {
		t.Fatal(err)
	}

	// (2910 + 30 teardown) s = 0.81666 h x 3 workers (max+1) x $0.72/h = $1.764,
	// x 2 FlashBoot settings = $3.528.
	const perConfig, worst = 1.764, 3.528
	if len(est.Configs) != 2 {
		t.Fatalf("got %d configs, want 2 (one per flashboot setting)", len(est.Configs))
	}
	for i, fb := range []string{"OFF", "FLASHBOOT"} {
		c := est.Configs[i]
		if c.Pool != "AMPERE_16" || c.FlashBoot != fb || c.PriceHr != 0.72 || !near(c.WorstCaseUSD, perConfig) {
			t.Errorf("Configs[%d] = %+v, want AMPERE_16/%s at $%v", i, c, fb, perConfig)
		}
	}
	if !near(est.WorstCaseUSD, worst) {
		t.Errorf("WorstCaseUSD = %v, want %v", est.WorstCaseUSD, worst)
	}
	// The budget guard caps spend at the budget.
	if est.CapUSD != 2.00 || est.MaxSpendUSD != 2.00 {
		t.Errorf("CapUSD, MaxSpendUSD = %v, %v, want 2, 2", est.CapUSD, est.MaxSpendUSD)
	}

	p.FlashBoot = []string{"OFF"}
	est, _ = EstimateRun(p, DefaultTimeouts(), poolsAt(prices))
	if !near(est.MaxSpendUSD, perConfig) {
		t.Errorf("one config: MaxSpendUSD = %v, want %v", est.MaxSpendUSD, perConfig)
	}
}

// Worked by hand with the real list prices, $2 budget, maxWorkers 2 (billed
// as 3) and a 1 s tick + 30 s teardown tail.
func TestEstimate_GuardStopsAfter(t *testing.T) {
	prices := map[string]float64{"AMPERE_16": 0.58, "AMPERE_24": 0.75}
	tests := []struct {
		name   string
		pools  []string
		budget float64
		want   int
	}{
		// 4 configs: 3 x (0.58+0.75) x 2 = $7.98/h = $0.0022167/s.
		// $2 / that = 902.26 s, minus 31 s = 871 s (~14.5 min).
		{"2 pools", []string{"AMPERE_16", "AMPERE_24"}, 2, 871},
		// 2 configs: 3 x 0.75 x 2 = $4.50/h. 1600 s - 31 s = 1569 s.
		{"AMPERE_24", []string{"AMPERE_24"}, 2, 1569},
		// $0.01 lasts 4.5 s at $7.98/h: less than the tail, so 0.
		{"never negative", []string{"AMPERE_16", "AMPERE_24"}, 0.01, 0},
	}
	for _, tt := range tests {
		p := validParams()
		p.Pools, p.BudgetUSD = tt.pools, tt.budget
		p.MaxWorkers = 2
		est, err := EstimateRun(p, DefaultTimeouts(), poolsAt(prices))
		if err != nil {
			t.Fatal(err)
		}
		if est.GuardStopsAfterS != tt.want {
			t.Errorf("%s: GuardStopsAfterS = %d, want %d", tt.name, est.GuardStopsAfterS, tt.want)
		}
	}
}

// Forced mode adds restoreTimeout (15 s) to the billed tail and the guard's projection.
func TestEstimate_ForcedAddsRestoreTail(t *testing.T) {
	p := validParams()
	p.FlashBoot = []string{"OFF"}
	p.ColdStartMode = ColdForced
	p.MaxWorkers = 2 // billed as 3, as in TestEstimate
	est, err := EstimateRun(p, DefaultTimeouts(), poolsAt(map[string]float64{"AMPERE_16": 0.72}))
	if err != nil {
		t.Fatal(err)
	}
	// (2910 + 30 + 15) s = 0.820833 h x 3 workers x $0.72/h = $1.773.
	if !near(est.WorstCaseUSD, 1.773) {
		t.Errorf("WorstCaseUSD = %v, want 1.773", est.WorstCaseUSD)
	}
	// 3 x 0.72 = $2.16/h: $2 lasts 3333.3 s, minus the 1 + 30 + 15 s tail.
	if est.GuardStopsAfterS != 3287 {
		t.Errorf("GuardStopsAfterS = %d, want 3287", est.GuardStopsAfterS)
	}
}

func TestEstimate_UnknownPool(t *testing.T) {
	p := validParams()
	p.Pools = []string{"AMPERE_16", "NOPE_99"}
	_, err := EstimateRun(p, DefaultTimeouts(), poolsAt(map[string]float64{"AMPERE_16": 0.58}))
	if err == nil || !strings.Contains(err.Error(), "NOPE_99") {
		t.Errorf("EstimateRun = %v, want an error naming NOPE_99", err)
	}
}

func TestEstimate_EmptyConfigsEncodeAsArray(t *testing.T) {
	est, _ := EstimateRun(Params{}, DefaultTimeouts(), nil)
	b, _ := json.Marshal(est)
	if !strings.Contains(string(b), `"configs":[]`) {
		t.Errorf("json = %s, want \"configs\":[]", b)
	}
}

func near(a, b float64) bool { return math.Abs(a-b) < 1e-9 }

func TestValidate_MatmulN(t *testing.T) {
	tests := []struct {
		name    string
		image   string
		matmulN int
		want    string // text the error must contain; "" means no error
	}{
		{"off on the synthetic image", SyntheticImage, 0, ""},
		{"on with the GPU image", GPUImage, 4096, ""},
		{"max", GPUImage, 8192, ""},
		{"above max", GPUImage, 8193, "matmulN: 8193 must be between 0 and 8192"},
		{"negative", GPUImage, -1, "matmulN: -1 must be between 0 and 8192"},
		// The synthetic image has no torch: every matmul job would fail.
		{"on with the synthetic image", SyntheticImage, 4096, "matmulN: needs the GPU image " + GPUImage},
		// Any tag or digest of the synthetic repo, named in the error as given.
		{"synthetic sha tag", "ghcr.io/josh10111/coldprobe-worker:sha-abc1234", 4096,
			"matmulN: needs the GPU image " + GPUImage + "; ghcr.io/josh10111/coldprobe-worker:sha-abc1234 only sleeps"},
		{"synthetic digest", "ghcr.io/josh10111/coldprobe-worker@sha256:0123abcd", 4096, "matmulN: needs the GPU image"},
		{"synthetic, no tag", "ghcr.io/josh10111/coldprobe-worker", 4096, "matmulN: needs the GPU image"},
		{"synthetic, padded", " " + SyntheticImage + " ", 4096, "matmulN: needs the GPU image"},
		// The GPU repo shares the prefix but isn't the synthetic image.
		{"GPU sha tag", "ghcr.io/josh10111/coldprobe-worker-gpu:sha-abc1234", 4096, ""},
		{"GPU digest", "ghcr.io/josh10111/coldprobe-worker-gpu@sha256:0123abcd", 4096, ""},
		{"on with no image", "", 4096, "image is required"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			p := validParams()
			p.Image, p.MatmulN = tt.image, tt.matmulN
			err := p.Validate(2, DefaultMaxWorkersLimit, fakeCatalog())
			if tt.want == "" {
				if err != nil {
					t.Fatalf("Validate() = %v, want nil", err)
				}
				return
			}
			if err == nil || !strings.Contains(err.Error(), tt.want) {
				t.Errorf("Validate() = %v, want it to contain %q", err, tt.want)
			}
		})
	}
}

// matmulN is omitempty, so runs without it keep the same Params JSON.
func TestParams_MatmulNOmittedWhenZero(t *testing.T) {
	b, _ := json.Marshal(validParams())
	if strings.Contains(string(b), "matmulN") {
		t.Errorf("json = %s, want no matmulN", b)
	}
	p := validParams()
	p.MatmulN = 4096
	b, _ = json.Marshal(p)
	if !strings.Contains(string(b), `"matmulN":4096`) {
		t.Errorf("json = %s, want \"matmulN\":4096", b)
	}
}

// fakeCatalog is what Mgmt.Pools returns for the two fakerunpod pools the
// tests use, kept by hand so a test reads the catalog it relies on.
func fakeCatalog() []runpod.Pool {
	return []runpod.Pool{
		{ID: "AMPERE_16", MemoryGB: 16, PricePerHr: 0.28, Availability: "HIGH", Cards: []runpod.Card{
			{ID: "NVIDIA RTX 2000 Ada Generation", Name: "RTX 2000 Ada", MemoryGB: 16, PriceHr: 0.24, Availability: "HIGH"},
			{ID: "NVIDIA RTX A4000", Name: "RTX A4000", MemoryGB: 16, PriceHr: 0.25, Availability: "MEDIUM"},
			{ID: "NVIDIA RTX A4500", Name: "RTX A4500", MemoryGB: 20, PriceHr: 0.25, Availability: "LOW"},
			{ID: "NVIDIA RTX 4000 Ada Generation", Name: "RTX 4000 Ada", MemoryGB: 20, PriceHr: 0.28, Availability: "LOW"},
		}},
		{ID: "AMPERE_24", MemoryGB: 24, PricePerHr: 0.69, Availability: "HIGH", Cards: []runpod.Card{
			{ID: "NVIDIA RTX A5000", Name: "RTX A5000", MemoryGB: 24, PriceHr: 0.27, Availability: "MEDIUM"},
			{ID: "NVIDIA GeForce RTX 3090", Name: "RTX 3090", MemoryGB: 24, PriceHr: 0.5, Availability: "HIGH"},
			{ID: "NVIDIA L4", Name: "L4", MemoryGB: 24, PriceHr: 0.59, Availability: "NONE"},
			{ID: "NVIDIA RTX PRO 6000 Blackwell Server Edition MIG 1g.24gb", Name: "PRO 6000 MIG 24GB", MemoryGB: 24, PriceHr: 0.69, Availability: "MEDIUM"},
		}},
	}
}

// poolsAt is a catalog of one-card pools at the given prices.
func poolsAt(prices map[string]float64) []runpod.Pool {
	var out []runpod.Pool
	for id, price := range prices {
		card := runpod.Card{ID: id + " GPU", PriceHr: price, Availability: "HIGH"}
		out = append(out, runpod.Pool{ID: id, PricePerHr: price, Availability: "HIGH", Cards: []runpod.Card{card}})
	}
	return out
}

// Card mode: each card in the catalog, at most two, never mixed with pools.
func TestValidate_CardsResolveToPool(t *testing.T) {
	tests := []struct {
		name   string
		modify func(p *Params)
		want   string // text the error must contain; "" means no error
	}{
		{"one card", func(p *Params) { p.Pools, p.Cards = nil, []string{"NVIDIA RTX A4500"} }, ""},
		// The UI's defaults send pools as [], which isn't "pools set".
		{"one card, pools empty", func(p *Params) { p.Pools, p.Cards = []string{}, []string{"NVIDIA RTX A4500"} }, ""},
		{"two cards, two pools", func(p *Params) { p.Pools, p.Cards = nil, []string{"NVIDIA RTX A4500", "NVIDIA L4"} }, ""},
		{"two cards, one pool", func(p *Params) { p.Pools, p.Cards = nil, []string{"NVIDIA RTX A4500", "NVIDIA RTX A4000"} }, ""},
		{"unknown card", func(p *Params) { p.Pools, p.Cards = nil, []string{"NVIDIA H100"} }, `cards: unknown card "NVIDIA H100"`},
		{"cards and pools", func(p *Params) { p.Cards = []string{"NVIDIA RTX A4500"} }, "cards: pick pools or cards, not both"},
		{"three cards", func(p *Params) {
			p.Pools, p.Cards = nil, []string{"NVIDIA RTX A4500", "NVIDIA RTX A4000", "NVIDIA L4"}
		}, "cards: pick 1 or 2, got 3"},
		{"duplicate card", func(p *Params) { p.Pools, p.Cards = nil, []string{"NVIDIA L4", "NVIDIA L4"} }, "cards: duplicate NVIDIA L4"},
		{"neither", func(p *Params) { p.Pools, p.Cards = nil, nil }, "pools: pick 1 or 2, got 0"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			p := validParams()
			tt.modify(&p)
			err := p.Validate(2, DefaultMaxWorkersLimit, fakeCatalog())
			if tt.want == "" {
				if err != nil {
					t.Fatalf("Validate() = %v, want nil", err)
				}
				return
			}
			if err == nil || !strings.Contains(err.Error(), tt.want) {
				t.Errorf("Validate() = %v, want it to contain %q", err, tt.want)
			}
		})
	}
}

// A card-mode config is billed at the card's own price and carries the
// card's stock level; a pool-mode config carries the pool's.
func TestEstimate_CardUsesCardPrice(t *testing.T) {
	catalog := []runpod.Pool{{ID: "MIXED_24", PricePerHr: 0.75, Availability: "HIGH", Cards: []runpod.Card{
		{ID: "NVIDIA CHEAP", PriceHr: 0.58, Availability: "LOW"},
		{ID: "NVIDIA DEAR", PriceHr: 0.75, Availability: "HIGH"},
	}}}
	p := validParams()
	p.Pools, p.Cards, p.FlashBoot = nil, []string{"NVIDIA CHEAP"}, []string{"OFF"}
	p.MaxWorkers = 2 // billed as 3, as in TestEstimate
	est, err := EstimateRun(p, DefaultTimeouts(), catalog)
	if err != nil {
		t.Fatal(err)
	}
	want := ConfigEstimate{Pool: "MIXED_24", Card: "NVIDIA CHEAP", FlashBoot: "OFF", PriceHr: 0.58, Availability: "LOW"}
	if len(est.Configs) != 1 {
		t.Fatalf("got %d configs, want 1", len(est.Configs))
	}
	got := est.Configs[0]
	got.WorstCaseUSD = 0
	if got != want {
		t.Errorf("config = %+v, want %+v", got, want)
	}
	// 2940 s x 3 workers x $0.58/h, as in TestEstimate.
	if !near(est.Configs[0].WorstCaseUSD, 2940.0/3600*3*0.58) {
		t.Errorf("WorstCaseUSD = %v, want the card's price", est.Configs[0].WorstCaseUSD)
	}

	p.Pools, p.Cards = []string{"MIXED_24"}, nil
	est, err = EstimateRun(p, DefaultTimeouts(), catalog)
	if err != nil {
		t.Fatal(err)
	}
	if c := est.Configs[0]; c.Card != "" || c.PriceHr != 0.75 || c.Availability != "HIGH" {
		t.Errorf("pool mode config = %+v, want no card, $0.75, HIGH", c)
	}

	p.Pools, p.Cards = nil, []string{"NVIDIA NOPE"}
	if _, err := EstimateRun(p, DefaultTimeouts(), catalog); err == nil || !strings.Contains(err.Error(), "NVIDIA NOPE") {
		t.Errorf("EstimateRun = %v, want an error naming NVIDIA NOPE", err)
	}
}

// A one-card pool has nothing to exclude, so CreateEndpoint omits
// gpu.excludedTypes.
func TestExcludedFor_SingleCardPoolIsEmpty(t *testing.T) {
	ada := runpod.Pool{ID: "ADA_24", Cards: []runpod.Card{{ID: "NVIDIA GeForce RTX 4090"}}}
	if got := ExcludedFor(ada, "NVIDIA GeForce RTX 4090"); len(got) != 0 {
		t.Errorf("ExcludedFor(ADA_24, 4090) = %v, want empty", got)
	}

	pool, card, ok := CardPool(fakeCatalog(), "NVIDIA RTX A4500")
	if !ok || pool.ID != "AMPERE_16" || card.Availability != "LOW" {
		t.Fatalf("CardPool(A4500) = %s, %+v, %v, want AMPERE_16, LOW, true", pool.ID, card, ok)
	}
	want := []string{"NVIDIA RTX 2000 Ada Generation", "NVIDIA RTX A4000", "NVIDIA RTX 4000 Ada Generation"}
	if got := ExcludedFor(pool, "NVIDIA RTX A4500"); !slices.Equal(got, want) {
		t.Errorf("ExcludedFor(AMPERE_16, A4500) = %v, want %v", got, want)
	}
	if _, _, ok := CardPool(fakeCatalog(), "NVIDIA H100"); ok {
		t.Error("CardPool(H100) found a pool, want ok false")
	}
}
