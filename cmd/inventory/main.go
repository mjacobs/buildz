// inventory: drive collect.sh against a list of hosts and persist results to Postgres.
//
// Usage:
//   inventory -hosts hosts.yaml [-host nas01] [-script collect.sh]
//
// Connection comes from $DATABASE_URL, e.g.
//   postgres://buildz:buildz@127.0.0.1:5433/inventory?sslmode=disable
package main

import (
	"bufio"
	"context"
	"encoding/json"
	"errors"
	"flag"
	"fmt"
	"log"
	"os"
	"os/exec"
	"regexp"
	"strconv"
	"strings"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"
	"gopkg.in/yaml.v3"
)

type HostEntry struct {
	Addr  string `yaml:"addr"`
	User  string `yaml:"user"`
	Label string `yaml:"label"`
}

type HostsFile struct {
	Hosts []HostEntry `yaml:"hosts"`
}

// Snapshot mirrors the JSON shape produced by collect.sh. Only the fields we
// normalize are typed; the rest stays in the raw JSONB column.
type Snapshot struct {
	SchemaVersion int    `json:"schema_version"`
	Hostname      string `json:"hostname"`
	Kernel        string `json:"kernel"`
	OSReleaseRaw  string `json:"os_release_raw"`
	CPUInfoRaw    string `json:"cpuinfo_raw"`
	MemInfoRaw    string `json:"meminfo_raw"`
	DMI           struct {
		SystemVendor  string `json:"system_vendor"`
		SystemProduct string `json:"system_product"`
		SystemSerial  string `json:"system_serial"`
		BoardVendor   string `json:"board_vendor"`
		BoardProduct  string `json:"board_product"`
		BoardSerial   string `json:"board_serial"`
		BIOSVendor    string `json:"bios_vendor"`
		BIOSVersion   string `json:"bios_version"`
		MemoryRaw     string `json:"memory_raw"`
	} `json:"dmi"`
	LSBLK   *LSBLK            `json:"lsblk"`
	IPAddr  []IPAddrEntry     `json:"ip_addr"`
	Ethtool map[string]string `json:"ethtool"`
	LSPCI     string       `json:"lspci_raw"`
	Smart     []SmartEntry `json:"smart"`
	NvidiaCSV string       `json:"nvidia_csv"`
}

type LSBLK struct {
	BlockDevices []LSBLKDevice `json:"blockdevices"`
}
type LSBLKDevice struct {
	Name   string `json:"name"`
	Type   string `json:"type"`
	Size   int64  `json:"size"`
	Model  string `json:"model"`
	Serial string `json:"serial"`
	Rota   bool   `json:"rota"`
	Tran   string `json:"tran"`
}

type IPAddrEntry struct {
	Ifname  string       `json:"ifname"`
	Address string       `json:"address"` // MAC
	AddrInfo []IPAddrInfo `json:"addr_info"`
}
type IPAddrInfo struct {
	Family    string `json:"family"`
	Local     string `json:"local"`
	Prefixlen int    `json:"prefixlen"`
	Scope     string `json:"scope"`
}

type SmartEntry struct {
	Device string          `json:"device"`
	Data   json.RawMessage `json:"data"`
}

func main() {
	var (
		hostsPath  = flag.String("hosts", "hosts.yaml", "path to hosts yaml")
		onlyHost   = flag.String("host", "", "only run for this label (or addr) from hosts.yaml")
		scriptPath = flag.String("script", "collect.sh", "path to collect.sh")
		timeout    = flag.Duration("timeout", 2*time.Minute, "per-host ssh timeout")
		fromFile   = flag.String("from-file", "", "ingest a previously-collected JSON file instead of running ssh (label via -host)")
	)
	flag.Parse()

	dsn := os.Getenv("DATABASE_URL")
	if dsn == "" {
		log.Fatal("DATABASE_URL is not set")
	}

	ctx := context.Background()
	pool, err := pgxpool.New(ctx, dsn)
	if err != nil {
		log.Fatalf("connect pg: %v", err)
	}
	defer pool.Close()
	if err := pool.Ping(ctx); err != nil {
		log.Fatalf("ping pg: %v", err)
	}

	if *fromFile != "" {
		raw, err := os.ReadFile(*fromFile)
		if err != nil {
			log.Fatalf("read snapshot: %v", err)
		}
		var snap Snapshot
		if err := json.Unmarshal(raw, &snap); err != nil {
			log.Fatalf("parse snapshot: %v", err)
		}
		h := HostEntry{Addr: snap.Hostname, Label: *onlyHost}
		if err := persist(ctx, pool, h, raw, &snap); err != nil {
			log.Fatalf("persist: %v", err)
		}
		log.Printf("ingested %s from %s", snap.Hostname, *fromFile)
		return
	}

	hostsRaw, err := os.ReadFile(*hostsPath)
	if err != nil {
		log.Fatalf("read hosts: %v", err)
	}
	var hf HostsFile
	if err := yaml.Unmarshal(hostsRaw, &hf); err != nil {
		log.Fatalf("parse hosts: %v", err)
	}
	script, err := os.ReadFile(*scriptPath)
	if err != nil {
		log.Fatalf("read script: %v", err)
	}

	var failures int
	for _, h := range hf.Hosts {
		if *onlyHost != "" && h.Label != *onlyHost && h.Addr != *onlyHost {
			continue
		}
		log.Printf("[%s] collecting…", displayName(h))
		raw, err := runRemote(h, script, *timeout)
		if err != nil {
			log.Printf("[%s] FAIL: %v", displayName(h), err)
			failures++
			continue
		}
		var snap Snapshot
		if err := json.Unmarshal(raw, &snap); err != nil {
			log.Printf("[%s] FAIL parse: %v", displayName(h), err)
			failures++
			continue
		}
		if err := persist(ctx, pool, h, raw, &snap); err != nil {
			log.Printf("[%s] FAIL persist: %v", displayName(h), err)
			failures++
			continue
		}
		log.Printf("[%s] ok (%s)", displayName(h), snap.Hostname)
	}
	if failures > 0 {
		os.Exit(1)
	}
}

func displayName(h HostEntry) string {
	if h.Label != "" {
		return h.Label
	}
	return h.Addr
}

// runRemote streams collect.sh into `ssh user@addr 'sudo bash -s'` and returns the JSON stdout.
func runRemote(h HostEntry, script []byte, timeout time.Duration) ([]byte, error) {
	ctx, cancel := context.WithTimeout(context.Background(), timeout)
	defer cancel()

	target := h.Addr
	if h.User != "" {
		target = h.User + "@" + h.Addr
	}
	cmd := exec.CommandContext(ctx, "ssh",
		"-o", "BatchMode=yes",
		"-o", "ConnectTimeout=10",
		target,
		"sudo -n bash -s",
	)
	cmd.Stdin = strings.NewReader(string(script))
	var stderr strings.Builder
	cmd.Stderr = &stderr
	out, err := cmd.Output()
	if err != nil {
		return nil, fmt.Errorf("ssh: %w; stderr=%s", err, strings.TrimSpace(stderr.String()))
	}
	return out, nil
}

// persist runs the whole upsert in one transaction so a host is either fully
// updated or left as it was.
func persist(ctx context.Context, pool *pgxpool.Pool, h HostEntry, raw []byte, s *Snapshot) error {
	tx, err := pool.Begin(ctx)
	if err != nil {
		return err
	}
	defer tx.Rollback(ctx)

	hostID, err := upsertHost(ctx, tx, s.Hostname, h.Label)
	if err != nil {
		return fmt.Errorf("upsert host: %w", err)
	}

	var collectionID int64
	err = tx.QueryRow(ctx,
		`INSERT INTO collections (host_id, schema_version, raw)
		 VALUES ($1, $2, $3) RETURNING id`,
		hostID, s.SchemaVersion, raw,
	).Scan(&collectionID)
	if err != nil {
		return fmt.Errorf("insert collection: %w", err)
	}

	if err := insertDisks(ctx, tx, hostID, collectionID, s); err != nil {
		return fmt.Errorf("disks: %w", err)
	}
	if err := insertNICs(ctx, tx, hostID, collectionID, s); err != nil {
		return fmt.Errorf("nics: %w", err)
	}
	if err := insertMemory(ctx, tx, hostID, collectionID, s); err != nil {
		return fmt.Errorf("memory: %w", err)
	}
	if err := insertGPUs(ctx, tx, hostID, collectionID, s); err != nil {
		return fmt.Errorf("gpus: %w", err)
	}
	if err := upsertHostCurrent(ctx, tx, hostID, collectionID, s); err != nil {
		return fmt.Errorf("host_current: %w", err)
	}

	return tx.Commit(ctx)
}

func upsertHost(ctx context.Context, tx pgx.Tx, hostname, label string) (int, error) {
	var id int
	err := tx.QueryRow(ctx, `
		INSERT INTO hosts (hostname, label)
		VALUES ($1, NULLIF($2, ''))
		ON CONFLICT (hostname) DO UPDATE
		   SET last_seen = now(),
		       label = COALESCE(NULLIF(EXCLUDED.label, ''), hosts.label)
		RETURNING id
	`, hostname, label).Scan(&id)
	return id, err
}

func insertDisks(ctx context.Context, tx pgx.Tx, hostID int, cid int64, s *Snapshot) error {
	if s.LSBLK == nil {
		return nil
	}
	smartByDev := map[string]json.RawMessage{}
	for _, e := range s.Smart {
		smartByDev[e.Device] = e.Data
	}
	for _, d := range s.LSBLK.BlockDevices {
		if d.Type != "disk" {
			continue
		}
		dev := "/dev/" + d.Name
		_, err := tx.Exec(ctx, `
			INSERT INTO disks (host_id, collection_id, device, model, serial, size_bytes, rota, tran, smart)
			VALUES ($1,$2,$3,$4,$5,$6,$7,$8,$9)
		`, hostID, cid, dev, nullStr(d.Model), nullStr(d.Serial), d.Size, d.Rota, nullStr(d.Tran), nullJSON(smartByDev[dev]))
		if err != nil {
			return err
		}
	}
	return nil
}

func insertNICs(ctx context.Context, tx pgx.Tx, hostID int, cid int64, s *Snapshot) error {
	speedRe := regexp.MustCompile(`Speed:\s*(\d+)\s*Mb/s`)
	driverRe := regexp.MustCompile(`(?m)^driver:\s*(\S+)`)
	for _, e := range s.IPAddr {
		if e.Ifname == "lo" || strings.HasPrefix(e.Ifname, "docker") || strings.HasPrefix(e.Ifname, "veth") {
			continue
		}
		var speed *int
		var driver *string
		if et, ok := s.Ethtool[e.Ifname]; ok {
			if m := speedRe.FindStringSubmatch(et); m != nil {
				if v, err := strconv.Atoi(m[1]); err == nil {
					speed = &v
				}
			}
			if m := driverRe.FindStringSubmatch(et); m != nil {
				d := m[1]
				driver = &d
			}
		}
		_, err := tx.Exec(ctx, `
			INSERT INTO nics (host_id, collection_id, ifname, mac, speed_mbps, driver, pci_id)
			VALUES ($1,$2,$3,$4,$5,$6,$7)
		`, hostID, cid, e.Ifname, nullStr(e.Address), speed, driver, nil)
		if err != nil {
			return err
		}
	}
	return nil
}

// insertMemory parses `dmidecode -t memory` output into per-DIMM rows.
// Skips empty slots (Size: "No Module Installed").
func insertMemory(ctx context.Context, tx pgx.Tx, hostID int, cid int64, s *Snapshot) error {
	if s.DMI.MemoryRaw == "" {
		return nil
	}
	scanner := bufio.NewScanner(strings.NewReader(s.DMI.MemoryRaw))
	scanner.Buffer(make([]byte, 1024*1024), 1024*1024)
	var (
		inDevice                                                  bool
		locator, mfr, partNum, serial                             string
		sizeBytes                                                 int64
		speedMTS                                                  int
	)
	flush := func() {
		if !inDevice {
			return
		}
		if sizeBytes > 0 {
			_, err := tx.Exec(ctx, `
				INSERT INTO memory_modules
				    (host_id, collection_id, locator, size_bytes, speed_mts, manufacturer, part_number, serial)
				VALUES ($1,$2,$3,$4,$5,$6,$7,$8)
			`, hostID, cid, nullStr(locator), sizeBytes, nullIntZero(speedMTS), nullStr(mfr), nullStr(partNum), nullStr(serial))
			if err != nil {
				log.Printf("memory insert: %v", err)
			}
		}
		inDevice, locator, mfr, partNum, serial, sizeBytes, speedMTS = false, "", "", "", "", 0, 0
	}
	for scanner.Scan() {
		line := scanner.Text()
		trim := strings.TrimSpace(line)
		if trim == "Memory Device" {
			flush()
			inDevice = true
			continue
		}
		if !inDevice {
			continue
		}
		if trim == "" {
			flush()
			continue
		}
		k, v, ok := strings.Cut(trim, ":")
		if !ok {
			continue
		}
		v = strings.TrimSpace(v)
		switch strings.TrimSpace(k) {
		case "Size":
			sizeBytes = parseDmiSize(v)
		case "Locator":
			locator = v
		case "Speed", "Configured Memory Speed":
			if speedMTS == 0 {
				speedMTS = parseLeadingInt(v)
			}
		case "Manufacturer":
			mfr = v
		case "Part Number":
			partNum = v
		case "Serial Number":
			serial = v
		}
	}
	flush()
	return scanner.Err()
}

// insertGPUs combines two sources:
//   - `nvidia-smi` CSV (name, pci.bus_id, memory.total): authoritative for NVIDIA + VRAM size.
//   - `lspci -vmmnn` records: catches AMD/Intel/etc and any NVIDIA cards the driver missed.
//
// PCI bus IDs are normalized so `00000000:01:00.0` (nvidia-smi) matches `01:00.0` (lspci).
func insertGPUs(ctx context.Context, tx pgx.Tx, hostID int, cid int64, s *Snapshot) error {
	type gpu struct {
		vendor, model, pciID string
		vram                 int64
	}
	seen := map[string]bool{}
	var gpus []gpu

	// nvidia-smi CSV: "name, pci.bus_id, memory.total"
	for _, line := range strings.Split(s.NvidiaCSV, "\n") {
		line = strings.TrimSpace(line)
		if line == "" {
			continue
		}
		parts := strings.SplitN(line, ",", 3)
		if len(parts) < 3 {
			continue
		}
		name := strings.TrimSpace(parts[0])
		bus := normalizePCI(strings.TrimSpace(parts[1]))
		var vram int64
		if mib := parseLeadingInt(strings.TrimSpace(parts[2])); mib > 0 {
			vram = int64(mib) * 1 << 20
		}
		gpus = append(gpus, gpu{vendor: "NVIDIA", model: name, pciID: bus, vram: vram})
		seen[bus] = true
	}

	// lspci fallback: parse the -vmmnn record blocks.
	for _, rec := range strings.Split(s.LSPCI, "\n\n") {
		fields := map[string]string{}
		for _, line := range strings.Split(rec, "\n") {
			k, v, ok := strings.Cut(line, ":")
			if !ok {
				continue
			}
			fields[strings.TrimSpace(k)] = strings.TrimSpace(v)
		}
		class := fields["Class"]
		if !strings.Contains(class, "VGA") && !strings.Contains(class, "3D controller") && !strings.Contains(class, "Display controller") {
			continue
		}
		bus := normalizePCI(fields["Slot"])
		if seen[bus] {
			continue
		}
		gpus = append(gpus, gpu{
			vendor: stripBracketID(fields["Vendor"]),
			model:  stripBracketID(fields["Device"]),
			pciID:  bus,
		})
		seen[bus] = true
	}

	for _, g := range gpus {
		_, err := tx.Exec(ctx, `
			INSERT INTO gpus (host_id, collection_id, vendor, model, pci_id, vram_bytes)
			VALUES ($1,$2,$3,$4,$5,$6)
		`, hostID, cid, nullStr(g.vendor), nullStr(g.model), nullStr(g.pciID), nullInt64Zero(g.vram))
		if err != nil {
			return err
		}
	}
	return nil
}

// normalizePCI turns "00000000:01:00.0" into "01:00.0" so different tools agree.
func normalizePCI(s string) string {
	s = strings.TrimSpace(s)
	if i := strings.LastIndex(s, ":"); i >= 0 {
		// Drop everything up to the second-to-last colon (the PCI domain).
		head := s[:i]
		if j := strings.Index(head, ":"); j >= 0 {
			return s[j+1:]
		}
	}
	return s
}

// stripBracketID removes the trailing PCI ID lspci appends, e.g.
// "NVIDIA Corporation [10de]" -> "NVIDIA Corporation".
func stripBracketID(s string) string {
	if i := strings.LastIndex(s, " ["); i >= 0 {
		return strings.TrimSpace(s[:i])
	}
	return strings.TrimSpace(s)
}

func upsertHostCurrent(ctx context.Context, tx pgx.Tx, hostID int, cid int64, s *Snapshot) error {
	cpuModel, sockets, cores, threads := parseCPUInfo(s.CPUInfoRaw)
	ramBytes := parseMemTotalKB(s.MemInfoRaw) * 1024
	osPretty := parseOSPretty(s.OSReleaseRaw)
	primaryMAC, primaryIP := pickPrimary(s.IPAddr)

	_, err := tx.Exec(ctx, `
		INSERT INTO host_current
		    (host_id, collection_id, os_pretty, kernel, cpu_model, cpu_sockets, cpu_cores, cpu_threads,
		     ram_bytes, board_vendor, board_product, board_serial, system_vendor, system_product, system_serial,
		     bios_vendor, bios_version, primary_mac, primary_ipv4, updated_at)
		VALUES ($1,$2,$3,$4,$5,$6,$7,$8,$9,$10,$11,$12,$13,$14,$15,$16,$17,$18,$19, now())
		ON CONFLICT (host_id) DO UPDATE SET
		    collection_id=EXCLUDED.collection_id, os_pretty=EXCLUDED.os_pretty, kernel=EXCLUDED.kernel,
		    cpu_model=EXCLUDED.cpu_model, cpu_sockets=EXCLUDED.cpu_sockets, cpu_cores=EXCLUDED.cpu_cores,
		    cpu_threads=EXCLUDED.cpu_threads, ram_bytes=EXCLUDED.ram_bytes,
		    board_vendor=EXCLUDED.board_vendor, board_product=EXCLUDED.board_product, board_serial=EXCLUDED.board_serial,
		    system_vendor=EXCLUDED.system_vendor, system_product=EXCLUDED.system_product, system_serial=EXCLUDED.system_serial,
		    bios_vendor=EXCLUDED.bios_vendor, bios_version=EXCLUDED.bios_version,
		    primary_mac=EXCLUDED.primary_mac, primary_ipv4=EXCLUDED.primary_ipv4, updated_at=now()
	`, hostID, cid, nullStr(osPretty), nullStr(s.Kernel), nullStr(cpuModel), nullIntZero(sockets), nullIntZero(cores), nullIntZero(threads),
		nullInt64Zero(ramBytes), nullStr(s.DMI.BoardVendor), nullStr(s.DMI.BoardProduct), nullStr(s.DMI.BoardSerial),
		nullStr(s.DMI.SystemVendor), nullStr(s.DMI.SystemProduct), nullStr(s.DMI.SystemSerial),
		nullStr(s.DMI.BIOSVendor), nullStr(s.DMI.BIOSVersion),
		nullStr(primaryMAC), nullStr(primaryIP))
	return err
}

// --- parsing helpers --------------------------------------------------------

func parseCPUInfo(raw string) (model string, sockets, cores, threads int) {
	physIDs := map[string]struct{}{}
	coresBySocket := map[string]int{}
	for _, line := range strings.Split(raw, "\n") {
		k, v, ok := strings.Cut(line, ":")
		if !ok {
			continue
		}
		k = strings.TrimSpace(k)
		v = strings.TrimSpace(v)
		switch k {
		case "model name":
			if model == "" {
				model = v
			}
		case "physical id":
			physIDs[v] = struct{}{}
		case "cpu cores":
			// Reported per logical CPU; same value within a socket.
			if n, err := strconv.Atoi(v); err == nil {
				// We'll just take the last seen; sockets * cores below.
				coresBySocket["any"] = n
			}
		case "processor":
			threads++
		}
	}
	sockets = len(physIDs)
	if sockets == 0 {
		sockets = 1
	}
	cores = sockets * coresBySocket["any"]
	if cores == 0 {
		cores = threads // fallback for VMs without physical id
	}
	return
}

func parseMemTotalKB(raw string) int64 {
	for _, line := range strings.Split(raw, "\n") {
		if strings.HasPrefix(line, "MemTotal:") {
			f := strings.Fields(line)
			if len(f) >= 2 {
				n, _ := strconv.ParseInt(f[1], 10, 64)
				return n
			}
		}
	}
	return 0
}

func parseOSPretty(raw string) string {
	for _, line := range strings.Split(raw, "\n") {
		if strings.HasPrefix(line, "PRETTY_NAME=") {
			return strings.Trim(strings.TrimPrefix(line, "PRETTY_NAME="), `"`)
		}
	}
	return ""
}

// pickPrimary returns the first non-loopback interface that has a global-scope IPv4.
func pickPrimary(entries []IPAddrEntry) (mac, ipv4 string) {
	for _, e := range entries {
		if e.Ifname == "lo" {
			continue
		}
		for _, a := range e.AddrInfo {
			if a.Family == "inet" && a.Scope == "global" {
				return e.Address, a.Local
			}
		}
	}
	return "", ""
}

// parseDmiSize converts strings like "16 GB", "8192 MB", "No Module Installed" to bytes.
func parseDmiSize(v string) int64 {
	if v == "" || strings.Contains(v, "No Module") {
		return 0
	}
	f := strings.Fields(v)
	if len(f) < 2 {
		return 0
	}
	n, err := strconv.ParseInt(f[0], 10, 64)
	if err != nil {
		return 0
	}
	switch strings.ToUpper(f[1]) {
	case "TB":
		return n * 1 << 40
	case "GB":
		return n * 1 << 30
	case "MB":
		return n * 1 << 20
	case "KB":
		return n * 1 << 10
	}
	return n
}

func parseLeadingInt(v string) int {
	f := strings.Fields(v)
	if len(f) == 0 {
		return 0
	}
	n, err := strconv.Atoi(f[0])
	if err != nil {
		return 0
	}
	return n
}

func nullStr(s string) any {
	if strings.TrimSpace(s) == "" {
		return nil
	}
	return s
}
func nullIntZero(n int) any {
	if n == 0 {
		return nil
	}
	return n
}
func nullInt64Zero(n int64) any {
	if n == 0 {
		return nil
	}
	return n
}
func nullJSON(b json.RawMessage) any {
	if len(b) == 0 || string(b) == "null" {
		return nil
	}
	return []byte(b)
}

var _ = errors.New // keep errors import alive if we add typed errors later
