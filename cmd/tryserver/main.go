// Command tryserver buys one server and reports whether its address is
// reachable from Iran. It never touches a config.
//
// Replacing a blocked server automatically needs a country that is known to be
// healthy, and during a broad blocking wave there is no such country — so the
// automatic path correctly refuses, and the only way to learn whether a fresh
// address still gets through is to buy one and probe it. That answer is the
// premise the whole project rests on, so it is worth being able to ask directly.
package main

import (
	"context"
	"flag"
	"fmt"
	"log/slog"
	"os"
	"sort"
	"strings"
	"time"

	"github.com/vefgh/botchecker/internal/checkhost"
	"github.com/vefgh/botchecker/internal/config"
	"github.com/vefgh/botchecker/internal/hetzner"
	"github.com/vefgh/botchecker/internal/scanner"
	"github.com/vefgh/botchecker/internal/settings"
	"github.com/vefgh/botchecker/internal/store"
)

func main() {
	var (
		location = flag.String("location", "", "Hetzner location (default: the configured one)")
		typ      = flag.String("type", "", "server type (default: the configured one)")
		ports    = flag.String("ports", "443,8443", "ports to probe once it is up")
		boot     = flag.Duration("boot", 90*time.Second, "how long to let it boot before probing")
		rounds   = flag.Int("rounds", 2, "probe rounds")
		delay    = flag.Duration("round-delay", 45*time.Second, "pause between rounds")
		keep     = flag.Bool("keep", true, "leave the server running (false deletes it at the end)")
		dry      = flag.Bool("dry-run", false, "show what would be created and stop")
	)
	flag.Parse()

	cfg, err := config.Load()
	if err != nil {
		die("configuration: %v", err)
	}
	log := slog.New(slog.NewTextHandler(os.Stdout, &slog.HandlerOptions{Level: slog.LevelWarn}))

	st, err := store.Open(cfg.DBPath)
	if err != nil {
		die("database: %v", err)
	}
	defer st.Close()

	set, err := settings.New(st.DB(), cfg.EncryptionKey, cfg.SecretKeyPath, nil)
	if err != nil {
		die("settings: %v", err)
	}

	hz := hetzner.New(set, log, cfg.HetznerTimeout, cfg.HetznerInventoryTTL)
	if !hz.Configured() {
		die("no Hetzner token is configured")
	}
	ch := checkhost.NewClient(cfg.CheckHostBaseURL, cfg.CheckHostRPSDelay, cfg.PollInterval, cfg.ResultTimeout)

	spec := hetzner.CreateSpec{
		Name:       fmt.Sprintf("%stry-%d", set.Get(settings.HetznerNamePrefix), time.Now().Unix()),
		ServerType: pick(*typ, set.Get(settings.HetznerServerType)),
		Image:      set.Get(settings.HetznerSnapshotID),
		Location:   pick(*location, set.Get(settings.HetznerLocation)),
		Labels:     map[string]string{"botchecker": "tryserver"},
	}
	if keys := strings.TrimSpace(set.Get(settings.HetznerSSHKeys)); keys != "" {
		spec.SSHKeys = strings.Split(keys, ",")
	}
	if spec.Image == "" {
		die("no snapshot id is configured")
	}

	fmt.Printf("about to create\n  name      %s\n  type      %s\n  location  %s\n  snapshot  %s\n\n",
		spec.Name, spec.ServerType, spec.Location, spec.Image)
	if *dry {
		fmt.Println("dry run — nothing was created")
		return
	}

	ctx := context.Background()
	created, err := hz.CreateFromSnapshot(ctx, spec)
	if err != nil {
		die("create: %v", err)
	}
	fmt.Printf("created id=%d ip=%s — waiting for the action to finish\n", created.ID, created.IPv4)

	if created.ActionID != 0 {
		if err := hz.WaitAction(ctx, created.ActionID, 5*time.Minute); err != nil {
			fmt.Printf("warning: %v\n", err)
		}
	}
	// The address is only on the API response once the server exists, so it is
	// re-read rather than trusted from the creation call.
	if res, err := hz.Server(ctx, created.ID); err == nil && res.Address != "" {
		created.IPv4 = res.Address
	}
	fmt.Printf("server is up: %s (%s)\n\nletting it boot for %s\n", created.IPv4, created.Name, *boot)
	time.Sleep(*boot)

	// Delete only what this run created, and only when asked.
	defer func() {
		if *keep {
			fmt.Printf("\nleaving it running: id=%d ip=%s\n", created.ID, created.IPv4)
			fmt.Printf("delete it with: curl -X DELETE -H \"Authorization: Bearer $HETZNER_TOKEN\" https://api.hetzner.cloud/v1/servers/%d\n", created.ID)
			return
		}
		fmt.Printf("\ndeleting id=%d\n", created.ID)
		if err := hz.DeleteServer(context.Background(), created.ID); err != nil {
			fmt.Printf("could not delete it — do it by hand: %v\n", err)
			return
		}
		fmt.Println("deleted")
	}()

	worked := false
	for _, p := range strings.Split(*ports, ",") {
		p = strings.TrimSpace(p)
		if p == "" {
			continue
		}
		if probe(ctx, ch, cfg, created.IPv4, p, *rounds, *delay) {
			worked = true
		}
	}

	fmt.Println()
	if worked {
		fmt.Printf("VERDICT: %s answers from Iran. A fresh address still gets through.\n", created.IPv4)
	} else {
		fmt.Printf("VERDICT: %s does not answer from Iran on any probed port.\n", created.IPv4)
		fmt.Println("If the control nodes were open, the machine is fine and the address is being filtered;")
		fmt.Println("if they were not, the service inside the snapshot is not listening yet.")
	}
}

// probe checks one port and prints what every node saw, because the per-node
// detail is the evidence — a summary cannot tell filtering from a dead service.
func probe(ctx context.Context, ch *checkhost.Client, cfg *config.Config, address, port string, rounds int, delay time.Duration) bool {
	hostPort := address + ":" + port
	irNodes := map[string]bool{}
	for _, n := range cfg.IRNodes {
		irNodes[n] = true
	}

	for round := 1; round <= rounds; round++ {
		fmt.Printf("\n── %s, round %d/%d ──\n", hostPort, round, rounds)

		check, err := ch.CheckTCP(ctx, hostPort, cfg.AllNodes())
		if err != nil {
			fmt.Printf("  probe failed: %v\n", err)
			continue
		}
		assess := scanner.Classify(check.Results, irNodes, cfg.MinIRFail, cfg.ControlEnabled())

		rows := append([]checkhost.NodeResult(nil), check.Results...)
		sort.Slice(rows, func(i, j int) bool { return rows[i].Node < rows[j].Node })
		for _, r := range rows {
			detail := string(r.Outcome)
			if r.RTTms > 0 {
				detail = fmt.Sprintf("%s %.0fms", r.Outcome, r.RTTms)
			}
			fmt.Printf("  %-6s %-8s %-9s %-10s %s\n",
				checkhost.ShortName(r.Node), scopeOf(r.Node, irNodes), r.City, r.ASN, detail)
		}
		fmt.Printf("  → %s  (%d/%d Iranian nodes open)\n", assess.Verdict, assess.IROpen, assess.IRTotal)

		if assess.Verdict == scanner.VerdictHealthy {
			return true
		}
		if round < rounds {
			time.Sleep(delay)
		}
	}
	return false
}

func scopeOf(node string, ir map[string]bool) string {
	if ir[node] {
		return "iran"
	}
	return "control"
}

func pick(override, fallback string) string {
	if strings.TrimSpace(override) != "" {
		return override
	}
	return fallback
}

func die(format string, args ...any) {
	fmt.Fprintf(os.Stderr, format+"\n", args...)
	os.Exit(1)
}
