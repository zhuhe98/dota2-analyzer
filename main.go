package main

import (
	"encoding/json"
	"fmt"
	"io"
	"os"
	"regexp"
	"sort"
	"strings"

	"github.com/dotabuff/manta"
	"github.com/dotabuff/manta/dota"
)

// ---- Output structures ----

type MatchOutput struct {
	MatchID        uint64      `json:"match_id"`
	Duration       float32     `json:"duration_seconds"`
	WinningTeam    string      `json:"winning_team"`
	Players        []Player    `json:"players"`
	KillEvents     []KillEvent `json:"kill_events"`
	GoldTimeline   []TeamGold  `json:"gold_timeline"`
	RadiantGoldAdv []int       `json:"radiant_gold_adv"`
	RadiantXpAdv   []int       `json:"radiant_xp_adv"`
	Teamfights     []Teamfight `json:"teamfights,omitempty"`
	Objectives     []Objective `json:"objectives,omitempty"`
}

type Position struct {
	T int `json:"t"`
	X int `json:"x"`
	Y int `json:"y"`
}

type Player struct {
	Slot        int    `json:"slot"`
	Team        string `json:"team"`
	HeroName    string `json:"hero_name"`
	PlayerName  string `json:"player_name"`
	SteamID     uint64 `json:"steam_id,omitempty"`
	Kills       int    `json:"kills"`
	Deaths      int    `json:"deaths"`
	Assists     int    `json:"assists"`
	LastHits    int    `json:"last_hits"`
	Denies      int    `json:"denies"`
	NetWorth    int    `json:"net_worth"`
	HeroDamage  int    `json:"hero_damage"`
	TowerDamage int    `json:"tower_damage"`
	HeroHealing int    `json:"hero_healing"`
	Level       int    `json:"level"`
	TotalGold   int    `json:"total_gold_earned"`
	TotalXP     int    `json:"total_xp_earned"`

	// Per-minute timelines
	Times []int `json:"times,omitempty"`
	GoldT []int `json:"gold_t,omitempty"`
	XpT   []int `json:"xp_t,omitempty"`
	LhT   []int `json:"lh_t,omitempty"`
	DnT   []int `json:"dn_t,omitempty"`

	// Coordinate tracking
	Positions []Position `json:"positions,omitempty"`

	// Combat event aggregates (per-source/target/ability)
	Damage          map[string]int `json:"damage,omitempty"`
	DamageTaken     map[string]int `json:"damage_taken,omitempty"`
	DamageInflictor map[string]int `json:"damage_inflictor,omitempty"`
	Healing         map[string]int `json:"healing,omitempty"`
	Killed          map[string]int `json:"killed,omitempty"`
	KilledBy        map[string]int `json:"killed_by,omitempty"`
	AbilityUses     map[string]int `json:"ability_uses,omitempty"`
	ItemUses        map[string]int `json:"item_uses,omitempty"`
	Purchase        map[string]int `json:"purchase,omitempty"`
	MultiKills      map[string]int `json:"multi_kills,omitempty"`
	KillStreaks     map[string]int `json:"kill_streaks,omitempty"`

	// Chronological logs
	PurchaseLog []PurchaseEntry `json:"purchase_log,omitempty"`
	KillsLog    []KillLogEntry  `json:"kills_log,omitempty"`
	BuybackLog  []BuybackEntry  `json:"buyback_log,omitempty"`
}

type PurchaseEntry struct {
	Time int    `json:"time"`
	Key  string `json:"key"`
}

type KillLogEntry struct {
	Time int    `json:"time"`
	Key  string `json:"key"`
}

type BuybackEntry struct {
	Time int `json:"time"`
}

type KillEvent struct {
	GameTime float32  `json:"game_time"`
	Killer   string   `json:"killer"`
	Victim   string   `json:"victim"`
	Assists  []string `json:"assists,omitempty"`
}

type TeamGold struct {
	GameTime    float32 `json:"game_time"`
	RadiantGold int     `json:"radiant_gold"`
	DireGold    int     `json:"dire_gold"`
}

type Teamfight struct {
	Start     int `json:"start"`
	End       int `json:"end"`
	LastDeath int `json:"last_death"`
	Deaths    int `json:"deaths"`
}

type Objective struct {
	Time int    `json:"time"`
	Type string `json:"type"`
	Team string `json:"team,omitempty"`
	Key  string `json:"key,omitempty"`
}

// ---- Player slot mapping ----

type playerEntry struct {
	dataIdx int
	slot    int
	team    int
}

type slotMap struct {
	dataToDisplay   [10]int
	slotToDisplay   map[int]int
	radiantDataIdxs []int
	direDataIdxs    []int
	resolved        bool
}

func buildSlotMap(entries []playerEntry) slotMap {
	sm := slotMap{slotToDisplay: make(map[int]int)}
	for i := range sm.dataToDisplay {
		sm.dataToDisplay[i] = i
	}

	var radiant, dire []playerEntry
	for _, e := range entries {
		switch e.team {
		case 2:
			radiant = append(radiant, e)
		case 3:
			dire = append(dire, e)
		}
	}

	if len(radiant) != 5 || len(dire) != 5 {
		return sm
	}

	sort.Slice(radiant, func(i, j int) bool { return radiant[i].slot < radiant[j].slot })
	sort.Slice(dire, func(i, j int) bool { return dire[i].slot < dire[j].slot })

	for i, p := range radiant {
		sm.dataToDisplay[p.dataIdx] = i
		sm.slotToDisplay[p.slot] = i
	}
	for i, p := range dire {
		sm.dataToDisplay[p.dataIdx] = 5 + i
		sm.slotToDisplay[p.slot] = 5 + i
	}

	all := make([]playerEntry, len(entries))
	copy(all, entries)
	sort.Slice(all, func(i, j int) bool { return all[i].dataIdx < all[j].dataIdx })
	for _, p := range all {
		if p.team == 2 {
			sm.radiantDataIdxs = append(sm.radiantDataIdxs, p.dataIdx)
		} else if p.team == 3 {
			sm.direDataIdxs = append(sm.direDataIdxs, p.dataIdx)
		}
	}

	sm.resolved = true
	return sm
}

// ---- Hero death record for teamfight detection ----

type heroDeath struct {
	gameTime int
	slot     int
}

// ---- Parser state ----

type State struct {
	parser          *manta.Parser
	outputPath      string
	output          MatchOutput
	killEvents      []KillEvent
	goldSamples     []TeamGold
	lastGoldTime    float32
	gameStartTime   float32
	gameEndTime     float32
	currentGameTime float32
	lastMinuteSampled int
	lastPositionTime  float32

	sm         slotMap
	rawEntries []playerEntry

	// Hero entities per slot for real-time sampling
	heroEntities map[int]*manta.Entity

	// Combat log name string table cache
	nameCache map[uint32]string

	// Hero combat-log-name → display slot (built from entity reads)
	heroNameToSlot map[string]int

	// Hero deaths for post-parse teamfight detection
	heroDeaths []heroDeath
}

func newState(p *manta.Parser) *State {
	s := &State{
		parser:         p,
		nameCache:      make(map[uint32]string),
		heroNameToSlot: make(map[string]int),
		heroEntities:   make(map[int]*manta.Entity),
	}
	s.output.Players = make([]Player, 10)
	for i := range s.output.Players {
		pl := &s.output.Players[i]
		pl.Slot = i
		if i < 5 {
			pl.Team = "radiant"
		} else {
			pl.Team = "dire"
		}
		pl.Damage = make(map[string]int)
		pl.DamageTaken = make(map[string]int)
		pl.DamageInflictor = make(map[string]int)
		pl.Healing = make(map[string]int)
		pl.Killed = make(map[string]int)
		pl.KilledBy = make(map[string]int)
		pl.AbilityUses = make(map[string]int)
		pl.ItemUses = make(map[string]int)
		pl.Purchase = make(map[string]int)
		pl.MultiKills = make(map[string]int)
		pl.KillStreaks = make(map[string]int)
	}
	s.sm.slotToDisplay = make(map[int]int)
	for i := range s.sm.dataToDisplay {
		s.sm.dataToDisplay[i] = i
	}
	return s
}

// resolveName looks up a combat log name index in the string table.
func (s *State) resolveName(idx uint32) string {
	if name, ok := s.nameCache[idx]; ok {
		return name
	}
	name, ok := s.parser.LookupStringByIndex("CombatLogNames", int32(idx))
	if !ok {
		name = fmt.Sprintf("idx_%d", idx)
	}
	s.nameCache[idx] = name
	return name
}

// stripItemPrefix removes "item_" prefix and "npc_dota_hero_" prefix from names.
func stripItemPrefix(name string) string {
	if strings.HasPrefix(name, "item_") {
		return name[5:]
	}
	return name
}

func stripHeroPrefix(name string) string {
	name = strings.TrimPrefix(name, "npc_dota_hero_")
	name = strings.TrimPrefix(name, "npc_dota_")
	return name
}

// camelToSnake converts "WraithKing" → "wraith_king".
var camelRe = regexp.MustCompile(`([A-Z])`)

func camelToSnake(s string) string {
	result := camelRe.ReplaceAllStringFunc(s, func(m string) string {
		return "_" + strings.ToLower(m)
	})
	return strings.TrimPrefix(result, "_")
}

// heroEntityNameToCombatLog converts entity class suffix to combat log name.
// "Antimage" → "npc_dota_hero_antimage"
// "WraithKing" → both "npc_dota_hero_wraithking" and "npc_dota_hero_wraith_king"
func heroEntityNameToCombatLog(suffix string) []string {
	lower := strings.ToLower(suffix)
	snake := camelToSnake(suffix)
	names := []string{"npc_dota_hero_" + lower}
	if snake != lower {
		names = append(names, "npc_dota_hero_"+snake)
	}
	return names
}

// slotForHero finds display slot for a hero combat log name.
func (s *State) slotForHero(name string) (int, bool) {
	if slot, ok := s.heroNameToSlot[name]; ok {
		return slot, true
	}
	return -1, false
}

// gameTimeSec returns current game time in seconds (integer).
func (s *State) gameTimeSec() int {
	return int(s.currentGameTime)
}

func getIntRobust(e *manta.Entity, key string) (int, bool) {
	if v, ok := e.GetInt32(key); ok {
		return int(v), true
	}
	if v, ok := e.GetUint32(key); ok {
		return int(v), true
	}
	if v, ok := e.GetUint64(key); ok {
		return int(v), true
	}
	if v, ok := e.GetFloat32(key); ok {
		return int(v), true
	}
	return 0, false
}

func getFloatRobust(e *manta.Entity, key string) (float32, bool) {
	if v, ok := e.GetFloat32(key); ok {
		return v, true
	}
	if v, ok := getIntRobust(e, key); ok {
		return float32(v), true
	}
	return 0, false
}

// ---- main ----

func main() {
	if len(os.Args) < 2 {
		fmt.Fprintln(os.Stderr, "Usage: replay-parser [-o output.json] <replay.dem>")
		os.Exit(1)
	}

	outputPath := ""
	replayArg := os.Args[1]
	if os.Args[1] == "-o" {
		if len(os.Args) < 4 {
			fmt.Fprintln(os.Stderr, "Usage: replay-parser [-o output.json] <replay.dem>")
			os.Exit(1)
		}
		outputPath = os.Args[2]
		replayArg = os.Args[3]
	}

	f, err := os.Open(replayArg)
	if err != nil {
		fmt.Fprintf(os.Stderr, "Cannot open replay: %v\n", err)
		os.Exit(1)
	}
	defer f.Close()

	p, err := manta.NewStreamParser(f)
	if err != nil {
		fmt.Fprintf(os.Stderr, "Failed to create parser: %v\n", err)
		os.Exit(1)
	}

	s := newState(p)
	s.outputPath = outputPath

	// CDemoFileInfo: match ID and winner
	p.Callbacks.OnCDemoFileInfo(func(m *dota.CDemoFileInfo) error {
		gi := m.GetGameInfo()
		if gi == nil {
			return nil
		}
		di := gi.GetDota()
		if di == nil {
			return nil
		}
		s.output.MatchID = di.GetMatchId()
		switch di.GetGameWinner() {
		case 2:
			s.output.WinningTeam = "radiant"
		case 3:
			s.output.WinningTeam = "dire"
		}
		return nil
	})

	p.OnEntity(func(e *manta.Entity, op manta.EntityOp) error {
		switch e.GetClassName() {
		case "CDOTAGamerulesProxy":
			s.readGameRules(e)
		case "CDOTA_PlayerResource":
			s.readPlayerResource(e)
		case "CDOTA_DataRadiant":
			s.readDataTeam(e, true)
		case "CDOTA_DataDire":
			s.readDataTeam(e, false)
		}
		if strings.HasPrefix(e.GetClassName(), "CDOTA_Unit_Hero_") {
			s.readHeroEntity(e)
		}
		return nil
	})

	// Combat log via legacy game events (older replays)
	p.OnGameEvent("dota_combatlog", func(ge *manta.GameEvent) error {
		return s.handleCombatLogEvent(ge)
	})

	// Combat log via bulk messages (newer replays)
	p.Callbacks.OnCDOTAUserMsg_CombatLogBulkData(func(m *dota.CDOTAUserMsg_CombatLogBulkData) error {
		for _, entry := range m.GetCombatEntries() {
			ts := entry.GetTimestamp()
			s.handleCombatLogEntry(entry, ts)
		}
		return nil
	})

	var lastPosTick uint32
	p.Callbacks.OnCNETMsg_Tick(func(m *dota.CNETMsg_Tick) error {
		tick := m.GetTick()
		if lastPosTick == 0 || tick-lastPosTick >= 150 { // Roughly 5 seconds at 30tps
			lastPosTick = tick
			s.checkPositionSampleTick(tick)
		}
		return nil
	})

	if err := p.Start(); err != nil {
		fmt.Fprintf(os.Stderr, "Parse error: %v\n", err)
		os.Exit(1)
	}

	// Final gold sample
	if s.output.Duration > 0 {
		s.sampleGold(s.output.Duration)
	}

	// Post-process
	s.output.KillEvents = s.killEvents
	s.output.GoldTimeline = s.goldSamples
	s.output.Teamfights = s.detectTeamfights()

	// Remove players with no data
	var active []Player
	for _, pl := range s.output.Players {
		if pl.HeroName != "" || pl.PlayerName != "" {
			active = append(active, pl)
		}
	}
	if len(active) > 0 {
		s.output.Players = active
	}

	// Fill empty player names (non-ASCII stripped) with hero name or "unknown"
	for i := range s.output.Players {
		if s.output.Players[i].PlayerName == "" {
			if s.output.Players[i].HeroName != "" {
				s.output.Players[i].PlayerName = s.output.Players[i].HeroName
			} else {
				s.output.Players[i].PlayerName = "unknown"
			}
		}
	}

	var out io.Writer = os.Stdout
	if s.outputPath != "" {
		f, err := os.Create(s.outputPath)
		if err != nil {
			fmt.Fprintf(os.Stderr, "Cannot create output file: %v\n", err)
			os.Exit(1)
		}
		defer f.Close()
		out = f
	}
	enc := json.NewEncoder(out)
	enc.SetIndent("", "  ")
	_ = enc.Encode(s.output)
}

// ---- Entity readers ----

func (s *State) readGameRules(e *manta.Entity) {
	if t, ok := e.GetFloat32("m_pGameRules.m_flGameStartTime"); ok && t > 0 && s.gameStartTime == 0 {
		s.gameStartTime = t
	}
	if t, ok := e.GetFloat32("m_pGameRules.m_flGameEndTime"); ok && t > 0 {
		s.gameEndTime = t
	}
	if s.output.WinningTeam == "" {
		if winner, ok := getIntRobust(e, "m_pGameRules.m_nGameWinner"); ok {
			switch winner {
			case 2:
				s.output.WinningTeam = "radiant"
			case 3:
				s.output.WinningTeam = "dire"
			}
		}
	}
	if s.gameEndTime > 0 && s.gameStartTime > 0 {
		s.output.Duration = s.gameEndTime - s.gameStartTime
	}
}

func (s *State) readPlayerResource(e *manta.Entity) {
	var entries []playerEntry
	for i := 0; i < 10; i++ {
		idx := fmt.Sprintf("%04d", i)
		slot, slotOk := getIntRobust(e, fmt.Sprintf("m_vecPlayerData.%s.m_nPlayerSlot", idx))
		team, teamOk := getIntRobust(e, fmt.Sprintf("m_vecPlayerData.%s.m_iPlayerTeam", idx))
		if slotOk && teamOk && (team == 2 || team == 3) {
			entries = append(entries, playerEntry{
				dataIdx: i,
				slot:    slot,
				team:    team,
			})
		}
	}

	if len(entries) == 10 && !s.sm.resolved {
		s.sm = buildSlotMap(entries)
	}

	for i := 0; i < 10; i++ {
		idx := fmt.Sprintf("%04d", i)
		ds := s.sm.dataToDisplay[i]

		if name, ok := e.GetString(fmt.Sprintf("m_vecPlayerData.%s.m_iszPlayerName", idx)); ok && name != "" {
			// Strip non-ASCII to prevent JSON parsing bombs from mixed command-line encodings
			cleanName := regexp.MustCompile(`[^\x20-\x7E]`).ReplaceAllString(name, "")
			s.output.Players[ds].PlayerName = cleanName
		}
		if sid, ok := e.GetUint64(fmt.Sprintf("m_vecPlayerData.%s.m_iPlayerSteamID", idx)); ok && sid != 0 {
			s.output.Players[ds].SteamID = sid
		}
		if v, ok := getIntRobust(e, fmt.Sprintf("m_vecPlayerTeamData.%s.m_iKills", idx)); ok {
			s.output.Players[ds].Kills = v
		}
		if v, ok := getIntRobust(e, fmt.Sprintf("m_vecPlayerTeamData.%s.m_iDeaths", idx)); ok {
			s.output.Players[ds].Deaths = v
		}
		if v, ok := getIntRobust(e, fmt.Sprintf("m_vecPlayerTeamData.%s.m_iAssists", idx)); ok {
			s.output.Players[ds].Assists = v
		}
		if v, ok := getIntRobust(e, fmt.Sprintf("m_vecPlayerTeamData.%s.m_iLevel", idx)); ok && v > s.output.Players[ds].Level {
			s.output.Players[ds].Level = v
		}
	}
}

func (s *State) readDataTeam(e *manta.Entity, isRadiant bool) {
	var dataIdxs []int
	if isRadiant {
		dataIdxs = s.sm.radiantDataIdxs
	} else {
		dataIdxs = s.sm.direDataIdxs
	}

	for i := 0; i < 5; i++ {
		idx := fmt.Sprintf("%04d", i)

		slot := -1
		if i < len(dataIdxs) && s.sm.resolved {
			slot = s.sm.dataToDisplay[dataIdxs[i]]
		}
		if slot < 0 || slot >= 10 {
			if isRadiant {
				slot = i
			} else {
				slot = 5 + i
			}
		}

		if v, ok := getIntRobust(e, fmt.Sprintf("m_vecDataTeam.%s.m_iLastHitCount", idx)); ok {
			s.output.Players[slot].LastHits = v
		}
		if v, ok := getIntRobust(e, fmt.Sprintf("m_vecDataTeam.%s.m_iDenyCount", idx)); ok {
			s.output.Players[slot].Denies = v
		}
		if v, ok := getIntRobust(e, fmt.Sprintf("m_vecDataTeam.%s.m_iNetWorth", idx)); ok {
			s.output.Players[slot].NetWorth = v
		}
		if v, ok := e.GetFloat32(fmt.Sprintf("m_vecDataTeam.%s.m_flHeroDamage", idx)); ok {
			s.output.Players[slot].HeroDamage = int(v)
		} else if v, ok := getIntRobust(e, fmt.Sprintf("m_vecDataTeam.%s.m_iHeroDamage", idx)); ok {
			s.output.Players[slot].HeroDamage = v
		}
		if v, ok := e.GetFloat32(fmt.Sprintf("m_vecDataTeam.%s.m_flTowerDamage", idx)); ok {
			s.output.Players[slot].TowerDamage = int(v)
		} else if v, ok := getIntRobust(e, fmt.Sprintf("m_vecDataTeam.%s.m_iTowerDamage", idx)); ok {
			s.output.Players[slot].TowerDamage = v
		}
		if v, ok := e.GetFloat32(fmt.Sprintf("m_vecDataTeam.%s.m_fHealing", idx)); ok {
			s.output.Players[slot].HeroHealing = int(v)
		}
		if v, ok := getIntRobust(e, fmt.Sprintf("m_vecDataTeam.%s.m_iTotalEarnedGold", idx)); ok {
			s.output.Players[slot].TotalGold = v
		}
		if v, ok := getIntRobust(e, fmt.Sprintf("m_vecDataTeam.%s.m_iTotalEarnedXP", idx)); ok {
			s.output.Players[slot].TotalXP = v
		}
	}
}

func (s *State) readHeroEntity(e *manta.Entity) {
	playerID, ok := getIntRobust(e, "m_iPlayerID")
	if !ok {
		return
	}

	// Modern Dota 2: m_iPlayerID in hero entities = dataIdx * 2
	dataIdx := playerID / 2
	if dataIdx < 0 || dataIdx >= 10 {
		return
	}
	ds := s.sm.dataToDisplay[dataIdx]
	if ds < 0 || ds >= 10 {
		return
	}

	suffix := strings.TrimPrefix(e.GetClassName(), "CDOTA_Unit_Hero_")
	if suffix != "" {
		s.output.Players[ds].HeroName = suffix
		s.heroEntities[ds] = e // Save entity reference for periodic coordinate sampling
		for _, combatLogName := range heroEntityNameToCombatLog(suffix) {
			s.heroNameToSlot[combatLogName] = ds
		}
	}
	if lvl, ok := getIntRobust(e, "m_iCurrentLevel"); ok && lvl > s.output.Players[ds].Level {
		s.output.Players[ds].Level = lvl
	}
}

// ---- Per-minute timeline sampling ----

func (s *State) checkMinuteSample() {
	if s.gameStartTime == 0 || s.currentGameTime < 0 {
		return
	}
	minute := int(s.currentGameTime / 60)
	if minute > s.lastMinuteSampled {
		s.sampleMinute(minute)
		s.lastMinuteSampled = minute
	}
}

func (s *State) checkPositionSampleTick(tick uint32) {
	for ds, e := range s.heroEntities {
		// Extract baseline cell boundaries
		cellX, okX := getIntRobust(e, "CBodyComponent.m_cellX")
		cellY, okY := getIntRobust(e, "CBodyComponent.m_cellY")
		if !okX || !okY {
			continue
		}

		// Extract local offsets within the cell
		vecX, okVX := getFloatRobust(e, "CBodyComponent.m_vecX")
		vecY, _ := getFloatRobust(e, "CBodyComponent.m_vecY") // Vector Y
		if okVX {
			// Translate into continuous global coordinates
			mappedX := int(float32(cellX)*128.0 + vecX)
			mappedY := int(float32(cellY)*128.0 + vecY)
			
			// Compute rough game time from tick or just use tick/30
			tSec := int(tick / 30)

			s.output.Players[ds].Positions = append(s.output.Players[ds].Positions, Position{
				T: tSec,
				X: mappedX,
				Y: mappedY,
			})
		}
	}
}

func (s *State) sampleMinute(minute int) {
	t := minute * 60
	radiantGold, direGold := 0, 0
	radiantXP, direXP := 0, 0

	for i := range s.output.Players {
		pl := &s.output.Players[i]
		pl.Times = append(pl.Times, t)
		pl.GoldT = append(pl.GoldT, pl.NetWorth)
		pl.XpT = append(pl.XpT, pl.TotalXP)
		pl.LhT = append(pl.LhT, pl.LastHits)
		pl.DnT = append(pl.DnT, pl.Denies)

		if i < 5 {
			radiantGold += pl.NetWorth
			radiantXP += pl.TotalXP
		} else {
			direGold += pl.NetWorth
			direXP += pl.TotalXP
		}
	}

	s.output.RadiantGoldAdv = append(s.output.RadiantGoldAdv, radiantGold-direGold)
	s.output.RadiantXpAdv = append(s.output.RadiantXpAdv, radiantXP-direXP)

	// Also record in gold timeline
	s.sampleGold(float32(t))
}

func (s *State) sampleGold(gt float32) {
	if gt <= s.lastGoldTime && gt > 0 {
		return
	}
	rGold, dGold := 0, 0
	for i, pl := range s.output.Players {
		if i < 5 {
			rGold += pl.NetWorth
		} else {
			dGold += pl.NetWorth
		}
	}
	if rGold > 0 || dGold > 0 {
		s.goldSamples = append(s.goldSamples, TeamGold{
			GameTime:    gt,
			RadiantGold: rGold,
			DireGold:    dGold,
		})
		s.lastGoldTime = gt
	}
}

// ---- Legacy combat log (older replays) ----

func (s *State) handleCombatLogEvent(ge *manta.GameEvent) error {
	t := ge.Type()
	if t == dota.DOTA_COMBATLOG_TYPES_DOTA_COMBATLOG_DEATH {
		victim, _ := ge.GetString("target_name")
		if !strings.Contains(victim, "npc_dota_hero") {
			return nil
		}
		killer, _ := ge.GetString("attacker_name")
		ts, _ := ge.GetFloat32("timestamp")
		gt := ts - s.gameStartTime
		s.killEvents = append(s.killEvents, KillEvent{
			GameTime: gt,
			Killer:   stripHeroPrefix(killer),
			Victim:   stripHeroPrefix(victim),
		})
	}
	return nil
}

// ---- Bulk combat log (newer replays) ----

func (s *State) handleCombatLogEntry(entry *dota.CMsgDOTACombatLogEntry, ts float32) {
	gt := ts - s.gameStartTime
	gts := int(gt)

	switch entry.GetType() {

	case dota.DOTA_COMBATLOG_TYPES_DOTA_COMBATLOG_DAMAGE:
		s.handleDamage(entry, gts)

	case dota.DOTA_COMBATLOG_TYPES_DOTA_COMBATLOG_HEAL:
		s.handleHeal(entry, gts)

	case dota.DOTA_COMBATLOG_TYPES_DOTA_COMBATLOG_DEATH:
		s.handleDeath(entry, gt, gts)

	case dota.DOTA_COMBATLOG_TYPES_DOTA_COMBATLOG_ABILITY:
		s.handleAbility(entry, gts)

	case dota.DOTA_COMBATLOG_TYPES_DOTA_COMBATLOG_ITEM:
		s.handleItemUse(entry, gts)

	case dota.DOTA_COMBATLOG_TYPES_DOTA_COMBATLOG_PURCHASE:
		s.handlePurchase(entry, gts)

	case dota.DOTA_COMBATLOG_TYPES_DOTA_COMBATLOG_BUYBACK:
		s.handleBuyback(entry, gts)

	case dota.DOTA_COMBATLOG_TYPES_DOTA_COMBATLOG_MULTIKILL:
		s.handleMultikill(entry, gts)

	case dota.DOTA_COMBATLOG_TYPES_DOTA_COMBATLOG_KILLSTREAK:
		s.handleKillstreak(entry, gts)

	case dota.DOTA_COMBATLOG_TYPES_DOTA_COMBATLOG_TEAM_BUILDING_KILL:
		s.handleBuildingKill(entry, gts)

	case dota.DOTA_COMBATLOG_TYPES_DOTA_COMBATLOG_FIRST_BLOOD:
		s.handleFirstblood(entry, gts)
	}
}

func (s *State) handleDamage(entry *dota.CMsgDOTACombatLogEntry, gts int) {
	if !entry.GetIsAttackerHero() || entry.GetIsAttackerIllusion() {
		return
	}
	attackerName := s.resolveName(entry.GetAttackerName())
	targetName := s.resolveName(entry.GetTargetName())
	inflictorName := stripItemPrefix(s.resolveName(entry.GetInflictorName()))
	val := int(entry.GetValue())
	if val <= 0 {
		return
	}

	attackerSlot, ok := s.slotForHero(attackerName)
	if !ok {
		return
	}
	ap := &s.output.Players[attackerSlot]

	// Damage dealt by attacker, keyed by target
	target := stripHeroPrefix(targetName)
	if target == "" {
		target = targetName
	}
	ap.Damage[target] += val
	if inflictorName != "" && inflictorName != "dota_unknown" {
		ap.DamageInflictor[inflictorName] += val
	}

	// Damage received by hero target
	if entry.GetIsTargetHero() && !entry.GetIsTargetIllusion() {
		if targetSlot, ok2 := s.slotForHero(targetName); ok2 {
			attacker := stripHeroPrefix(attackerName)
			s.output.Players[targetSlot].DamageTaken[attacker] += val
		}
	}
}

func (s *State) handleHeal(entry *dota.CMsgDOTACombatLogEntry, gts int) {
	if !entry.GetIsAttackerHero() || entry.GetIsAttackerIllusion() {
		return
	}
	attackerName := s.resolveName(entry.GetAttackerName())
	targetName := s.resolveName(entry.GetTargetName())
	val := int(entry.GetValue())
	if val <= 0 {
		return
	}

	attackerSlot, ok := s.slotForHero(attackerName)
	if !ok {
		return
	}
	target := stripHeroPrefix(targetName)
	if target == "" {
		target = targetName
	}
	s.output.Players[attackerSlot].Healing[target] += val
}

func (s *State) handleDeath(entry *dota.CMsgDOTACombatLogEntry, gt float32, gts int) {
	if !entry.GetIsTargetHero() || entry.GetIsTargetIllusion() {
		return
	}
	if gt < -30 {
		return
	}

	attackerName := s.resolveName(entry.GetAttackerName())
	targetName := s.resolveName(entry.GetTargetName())

	killer := stripHeroPrefix(attackerName)
	victim := stripHeroPrefix(targetName)

	// Collect assists
	var assists []string
	for _, ap := range entry.GetAssistPlayers() {
		if ap >= 0 && int(ap) < len(s.output.Players) {
			h := s.output.Players[ap].HeroName
			if h != "" {
				assists = append(assists, h)
			}
		}
	}

	s.killEvents = append(s.killEvents, KillEvent{
		GameTime: gt,
		Killer:   killer,
		Victim:   victim,
		Assists:  assists,
	})

	// Track kills_log for killer
	if attackerSlot, ok := s.slotForHero(attackerName); ok {
		ap := &s.output.Players[attackerSlot]
		ap.Killed[victim]++
		ap.KillsLog = append(ap.KillsLog, KillLogEntry{Time: gts, Key: victim})
	}

	// Track killed_by for victim
	if targetSlot, ok := s.slotForHero(targetName); ok {
		s.output.Players[targetSlot].KilledBy[killer]++
		// Record hero death for teamfight detection
		s.heroDeaths = append(s.heroDeaths, heroDeath{gameTime: gts, slot: targetSlot})
	}
}

func (s *State) handleAbility(entry *dota.CMsgDOTACombatLogEntry, gts int) {
	if !entry.GetIsAttackerHero() || entry.GetIsAttackerIllusion() {
		return
	}
	attackerName := s.resolveName(entry.GetAttackerName())
	abilityName := s.resolveName(entry.GetInflictorName())
	if abilityName == "" || abilityName == "dota_unknown" {
		return
	}

	attackerSlot, ok := s.slotForHero(attackerName)
	if !ok {
		return
	}
	s.output.Players[attackerSlot].AbilityUses[abilityName]++
}

func (s *State) handleItemUse(entry *dota.CMsgDOTACombatLogEntry, gts int) {
	if !entry.GetIsAttackerHero() || entry.GetIsAttackerIllusion() {
		return
	}
	attackerName := s.resolveName(entry.GetAttackerName())
	itemName := stripItemPrefix(s.resolveName(entry.GetInflictorName()))
	if itemName == "" || itemName == "dota_unknown" {
		return
	}

	attackerSlot, ok := s.slotForHero(attackerName)
	if !ok {
		return
	}
	s.output.Players[attackerSlot].ItemUses[itemName]++
}

func (s *State) handlePurchase(entry *dota.CMsgDOTACombatLogEntry, gts int) {
	buyerName := s.resolveName(entry.GetTargetName())
	itemName := stripItemPrefix(s.resolveName(entry.GetInflictorName()))
	if itemName == "" || itemName == "dota_unknown" || strings.HasPrefix(itemName, "recipe_") {
		return
	}

	buyerSlot, ok := s.slotForHero(buyerName)
	if !ok {
		return
	}
	p := &s.output.Players[buyerSlot]
	p.Purchase[itemName]++
	p.PurchaseLog = append(p.PurchaseLog, PurchaseEntry{Time: gts, Key: itemName})
}

func (s *State) handleBuyback(entry *dota.CMsgDOTACombatLogEntry, gts int) {
	// Value contains player slot index
	slot := int(entry.GetValue())
	if slot >= 0 && slot < 10 {
		s.output.Players[slot].BuybackLog = append(
			s.output.Players[slot].BuybackLog,
			BuybackEntry{Time: gts},
		)
	}
}

func (s *State) handleMultikill(entry *dota.CMsgDOTACombatLogEntry, gts int) {
	attackerName := s.resolveName(entry.GetAttackerName())
	killCount := fmt.Sprintf("%d", entry.GetValue())

	attackerSlot, ok := s.slotForHero(attackerName)
	if !ok {
		return
	}
	s.output.Players[attackerSlot].MultiKills[killCount]++
}

func (s *State) handleKillstreak(entry *dota.CMsgDOTACombatLogEntry, gts int) {
	attackerName := s.resolveName(entry.GetAttackerName())
	streakCount := fmt.Sprintf("%d", entry.GetValue())

	attackerSlot, ok := s.slotForHero(attackerName)
	if !ok {
		return
	}
	s.output.Players[attackerSlot].KillStreaks[streakCount]++
}

func (s *State) handleBuildingKill(entry *dota.CMsgDOTACombatLogEntry, gts int) {
	targetName := s.resolveName(entry.GetTargetName())
	attackerName := s.resolveName(entry.GetAttackerName())
	team := ""
	if strings.Contains(targetName, "goodguys") || strings.Contains(targetName, "radiant") {
		team = "radiant"
	} else if strings.Contains(targetName, "badguys") || strings.Contains(targetName, "dire") {
		team = "dire"
	}

	obj := Objective{
		Time: gts,
		Type: "building_kill",
		Team: team,
		Key:  targetName,
	}
	_ = attackerName
	s.output.Objectives = append(s.output.Objectives, obj)
}

func (s *State) handleFirstblood(entry *dota.CMsgDOTACombatLogEntry, gts int) {
	attackerName := s.resolveName(entry.GetAttackerName())
	targetName := s.resolveName(entry.GetTargetName())
	s.output.Objectives = append(s.output.Objectives, Objective{
		Time: gts,
		Type: "first_blood",
		Key:  stripHeroPrefix(attackerName) + " killed " + stripHeroPrefix(targetName),
	})
}

// ---- Teamfight detection ----

// detectTeamfights groups hero deaths within 15 seconds into teamfights
// and returns those with 3+ deaths.
func (s *State) detectTeamfights() []Teamfight {
	const cooldown = 15
	const minDeaths = 3

	if len(s.heroDeaths) == 0 {
		return nil
	}

	// Sort by game time
	sort.Slice(s.heroDeaths, func(i, j int) bool {
		return s.heroDeaths[i].gameTime < s.heroDeaths[j].gameTime
	})

	var teamfights []Teamfight
	var current *Teamfight

	for _, d := range s.heroDeaths {
		if current == nil {
			current = &Teamfight{
				Start:     d.gameTime - cooldown,
				LastDeath: d.gameTime,
				Deaths:    1,
			}
			if current.Start < 0 {
				current.Start = 0
			}
		} else if d.gameTime-current.LastDeath <= cooldown {
			current.LastDeath = d.gameTime
			current.Deaths++
		} else {
			current.End = current.LastDeath + cooldown
			if current.Deaths >= minDeaths {
				teamfights = append(teamfights, *current)
			}
			current = &Teamfight{
				Start:     d.gameTime - cooldown,
				LastDeath: d.gameTime,
				Deaths:    1,
			}
			if current.Start < 0 {
				current.Start = 0
			}
		}
	}
	if current != nil {
		current.End = current.LastDeath + cooldown
		if current.Deaths >= minDeaths {
			teamfights = append(teamfights, *current)
		}
	}

	return teamfights
}
