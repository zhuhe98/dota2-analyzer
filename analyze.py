#!/usr/bin/env python3
"""
Dota 2 Replay Coaching Analyzer — Phase-based, multi-request
Usage: python analyze.py <replay.dem> [player_name_or_number]
"""

import concurrent.futures
import json
import math
import os
import subprocess
import sys
import tempfile


# ── Map zone classification ───────────────────────────────────────────────────

def classify_zone(x, y):
    if x < 10800 and y < 11200:
        return "radiant_base"
    if x > 21800 and y > 20800:
        return "dire_base"
    cx, cy = x - 8192, y - 8192
    if abs(cx - cy) < 2400:
        return "mid_lane"
    if y < 13500:
        return "bot_lane"
    if y > 19000:
        return "top_lane"
    return "jungle"


# Column order for zone tables (absolute map zones, independent of team)
ZONE_COLS = ["radiant_base", "bot_lane", "mid_lane", "top_lane", "dire_base", "jungle"]
ZONE_HEAD  = ["RadiantBase", "Bot",       "Mid",       "Top",       "DireBase",   "Jungle"]


def zone_pct(positions):
    counts = {}
    for p in positions:
        z = classify_zone(p["x"], p["y"])
        counts[z] = counts.get(z, 0) + 1
    n = max(len(positions), 1)
    return {z: round(c / n * 100) for z, c in counts.items()}


def idle_pct(positions):
    if len(positions) < 2:
        return 0
    idle = sum(
        1 for i in range(1, len(positions))
        if math.hypot(positions[i]["x"] - positions[i-1]["x"],
                      positions[i]["y"] - positions[i-1]["y"]) < 50
    )
    return round(idle / (len(positions) - 1) * 100)


# ── Position phase splitting ──────────────────────────────────────────────────

PHASE_LABELS = ["Early Game (0-15 min)", "Mid Game (15-30 min)", "Late Game (30+ min)"]


def split_positions_by_phase(all_players):
    """Return 3 dicts: {hero_name → positions_slice} for each phase."""
    ref = next((p for p in all_players if p.get("positions")), None)
    if not ref:
        return [{} for _ in range(3)]
    n = len(ref["positions"])
    cuts = [n // 3, 2 * n // 3, n]
    phases = []
    for idx in range(3):
        start = 0 if idx == 0 else cuts[idx - 1]
        phases.append({p["hero_name"]: p["positions"][start:cuts[idx]] for p in all_players})
    return phases


# ── Metrics helpers ───────────────────────────────────────────────────────────

def kda(p):
    return round((p["kills"] + p["assists"]) / max(p["deaths"], 1), 2)

def gpm(p, dur_min):
    return round(p["total_gold_earned"] / dur_min)


# ── Table formatters ──────────────────────────────────────────────────────────

def fmt_final_stats_table(players, target, dur_min):
    hdr = (f"{'Hero':<22} {'Player':<18} {'Team':>6} "
           f"{'K':>3} {'D':>3} {'A':>3} {'LH':>5} {'NW':>7} "
           f"{'Dmg':>7} {'GPM':>5} {'KDA':>5}")
    rows = [hdr, "-" * len(hdr)]
    for team_tag in ["radiant", "dire"]:
        for p in sorted([x for x in players if x["team"] == team_tag],
                        key=lambda x: x["net_worth"], reverse=True):
            mk = " <<" if p is target else ""
            rows.append(
                f"{p['hero_name']:<22} {p['player_name']:<18} {team_tag:>6} "
                f"{p['kills']:>3} {p['deaths']:>3} {p['assists']:>3} "
                f"{p['last_hits']:>5} {p['net_worth']:>7} "
                f"{p['hero_damage']:>7} {gpm(p, dur_min):>5} {kda(p):>5}{mk}"
            )
    return "\n".join(rows)


def fmt_zone_table(all_players, phase_map, target_hero):
    """Zone distribution table for one phase — all players, both teams."""
    col_w = 11
    hdr = (f"{'Hero':<22} {'Player':<18} {'Team':>6} "
           + "".join(f"{h:>{col_w}}" for h in ZONE_HEAD)
           + f"  {'Idle%':>5}")
    rows = [hdr, "-" * len(hdr)]

    for team_tag in ["radiant", "dire"]:
        for p in sorted([x for x in all_players if x["team"] == team_tag],
                        key=lambda x: x["net_worth"], reverse=True):
            pos  = phase_map.get(p["hero_name"], [])
            zp   = zone_pct(pos)
            idle = idle_pct(pos)
            mk   = " <<" if p["hero_name"] == target_hero else ""
            cols = "".join(f"{zp.get(z, 0):>{col_w}}%" for z in ZONE_COLS)
            rows.append(
                f"{p['hero_name']:<22} {p['player_name']:<18} {team_tag:>6} "
                f"{cols}  {idle:>4}%{mk}"
            )
        rows.append("")  # blank line between teams

    return "\n".join(rows).rstrip()


# ── Shared match context (used in all 3 prompts) ──────────────────────────────

def build_shared_context(data, player, all_players):
    dur = data["duration_seconds"] / 60
    winning = data["winning_team"]
    team = player["team"]
    result = "WIN" if team == winning else "LOSS"

    allies  = [p for p in all_players if p["team"] == team]
    allies_by_nw = sorted(allies, key=lambda p: p["net_worth"], reverse=True)
    role_names = ["Carry (Pos 1)", "Mid (Pos 2)", "Offlane (Pos 3)",
                  "Support (Pos 4)", "Hard Support (Pos 5)"]
    role = role_names[min(allies_by_nw.index(player), 4)]

    return (
        f"Match ID: {data['match_id']}  |  Duration: {dur:.1f} min  |  "
        f"Winner: {winning.capitalize()}  |  Result for target: {result}\n\n"
        f"TARGET PLAYER: {player['player_name']} ({player['hero_name']}) "
        f"— {team.capitalize()}, {role}\n"
        f"Final stats: {player['kills']}/{player['deaths']}/{player['assists']} "
        f"(KDA {kda(player)}) | NW {player['net_worth']:,} | GPM {gpm(player,dur)} | "
        f"LH {player['last_hits']} | Dmg {player['hero_damage']:,} | Lvl {player['level']}\n\n"
        f"ALL PLAYERS — FINAL STATS:\n"
        f"{fmt_final_stats_table(all_players, player, dur)}"
    ), role


# ── Per-phase prompt builders ─────────────────────────────────────────────────

_PHASE_QUESTIONS = [
    # Early game
    """\
Focus ONLY on the EARLY GAME (laning phase).

1. LANING CORRECTNESS
   Based on zone distribution, was the target player in the right lane for their role ({role})?
   Compare with allies: who was where? Was lane coverage balanced?
   Compare with enemies: what were opponent heroes doing in the same phase?

2. ACTIVITY vs PASSIVITY
   Did the player roam, pull, or apply pressure early? Or stay in one zone passively?
   Reference specific zone percentages. Who on either team was most active/mobile?

3. EARLY GAME PROBLEMS
   Identify 2-3 concrete mistakes based on the zone data.
   Compare directly: "while X enemy was doing Y, target player was at Z% in [zone]."

4. EARLY GAME IMPROVEMENT
   2-3 specific, actionable tips for laning phase as {hero}.""",

    # Mid game
    """\
Focus ONLY on the MID GAME (post-laning, before late game).

1. TRANSITION QUALITY
   Did the player properly leave laning and start impacting the map?
   Compare their zone distribution vs allies: who was fighting, who was farming, who was rotating?
   Compare vs enemies: which side used mid-game better?

2. OBJECTIVE FOCUS
   Did the zone data suggest participation in towers / Roshan / teamfights (mid-lane, enemy base area)?
   Or excessive jungle farming? Give specific percentages.

3. RELATIVE IMPACT
   Find the enemy player in a similar role. Compare their mid-game positioning.
   Who had better map coverage? What can the target player learn from that?

4. MID GAME IMPROVEMENT
   2-3 specific tips for the mid-game phase as {hero} ({role}).""",

    # Late game
    """\
Focus ONLY on the LATE GAME (final phase of the match).

1. LATE POSITIONING
   Where was the target player during the late game? Was this appropriate for {hero} ({role})?
   Compare with allies: did the team group well, or was someone out of position?
   Compare with enemies: what did winning/losing come down to positionally?

2. DEATH RISK ASSESSMENT
   The target player died {deaths} times total. In the late game zone data, were they in high-risk areas?
   Which enemy heroes posed the biggest threat and how should positioning account for them?

3. GAME CLOSING / COMEBACK
   The winner was {winner}. Did the target player contribute to or fail in the final push?
   Reference zone data: enemy base %, idle%, movement.

4. LATE GAME IMPROVEMENT
   2-3 specific tips for closing/surviving late game as {hero} ({role}).""",
]


def build_phase_prompts(data, player):
    all_players = data["players"]
    shared_ctx, role = build_shared_context(data, player, all_players)
    phase_maps = split_positions_by_phase(all_players)

    prompts = []
    for label, phase_map, questions in zip(PHASE_LABELS, phase_maps, _PHASE_QUESTIONS):
        zone_table = fmt_zone_table(all_players, phase_map, player["hero_name"])
        q = questions.format(
            hero=player["hero_name"],
            role=role,
            deaths=player["deaths"],
            winner=data["winning_team"].capitalize(),
        )
        prompt = (
            f"You are an expert Dota 2 coach. Use the data below to give focused, phase-specific coaching.\n\n"
            f"{'='*55}\n"
            f"MATCH CONTEXT\n"
            f"{'='*55}\n"
            f"{shared_ctx}\n\n"
            f"{'='*55}\n"
            f"ZONE DISTRIBUTION — {label.upper()}\n"
            f"{'='*55}\n"
            f"Columns = % of time spent in each map zone during this phase only.\n"
            f"RadiantBase=Radiant base area | Bot=bottom lane | Mid=mid lane | "
            f"Top=top lane | DireBase=Dire base area | Jungle=jungle/roaming\n"
            f"Target player marked with <<\n\n"
            f"{zone_table}\n\n"
            f"{'='*55}\n"
            f"COACHING QUESTIONS — {label.upper()}\n"
            f"{'='*55}\n"
            f"{q}\n\n"
            f"Be specific and cite actual zone percentages when comparing players. "
            f"Output in Chinese. Keep each section concise."
        )
        prompts.append((label, prompt))

    return prompts


# ── Parser invocation ─────────────────────────────────────────────────────────

def run_parser(replay_path, parser_exe=None):
    if parser_exe is None:
        script_dir = os.path.dirname(os.path.abspath(__file__))
        for name in ("replay-parser.exe", "replay-parser", "replayparser.exe"):
            candidate = os.path.join(script_dir, name)
            if os.path.isfile(candidate):
                parser_exe = candidate
                break
        if parser_exe is None:
            sys.exit("ERROR: replay-parser binary not found next to analyze.py")

    with tempfile.NamedTemporaryFile(suffix=".json", delete=False) as tmp:
        tmp_path = tmp.name
    try:
        result = subprocess.run(
            [parser_exe, "-o", tmp_path, replay_path],
            capture_output=True, text=True,
        )
        if result.returncode != 0:
            sys.exit(f"ERROR: parser failed:\n{result.stderr}")
        with open(tmp_path, encoding="utf-8") as f:
            return json.load(f)
    finally:
        try:
            os.unlink(tmp_path)
        except OSError:
            pass


# ── OpenAI call ───────────────────────────────────────────────────────────────

def call_one_phase(phase_label, prompt, client, model):
    response = client.chat.completions.create(
        model=model,
        messages=[
            {"role": "system",
             "content": "You are an expert Dota 2 coach. Be specific, data-driven, and constructive. Output in Chinese."},
            {"role": "user", "content": prompt},
        ],
        temperature=0.4,
    )
    return phase_label, response.choices[0].message.content


# ── Main ──────────────────────────────────────────────────────────────────────

def main():
    if len(sys.argv) < 2:
        print("Usage: python analyze.py <replay.dem> [player_name_or_number]")
        sys.exit(1)

    replay_path = sys.argv[1]
    if not os.path.isfile(replay_path):
        sys.exit(f"ERROR: File not found: {replay_path}")

    model = os.environ.get("OPENAI_MODEL", "gpt-4o")

    print(f"Parsing replay: {replay_path}")
    data = run_parser(replay_path)

    players = data.get("players", [])
    if not players:
        sys.exit("ERROR: No players found in replay data.")

    print("\nPlayers in this match:")
    for i, p in enumerate(players):
        tag = "R" if p["team"] == "radiant" else "D"
        print(f"  [{i+1}] [{tag}] {p['player_name']} ({p['hero_name']})")

    # Select player
    if len(sys.argv) >= 3:
        sel = sys.argv[2]
    else:
        print("\nEnter player number or name: ", end="", flush=True)
        sel = input().strip()

    if sel.isdigit():
        idx = int(sel) - 1
        if not (0 <= idx < len(players)):
            sys.exit(f"ERROR: Number out of range (1-{len(players)})")
        selected = players[idx]
    else:
        matches = [p for p in players if sel.lower() in p["player_name"].lower()]
        if not matches:
            sys.exit(f"ERROR: No player matching '{sel}'")
        selected = matches[0]

    print(f"\nAnalyzing: {selected['player_name']} ({selected['hero_name']})")

    try:
        from openai import OpenAI
    except ImportError:
        sys.exit("ERROR: pip install openai")

    api_key = os.environ.get("OPENAI_API_KEY")
    if not api_key:
        sys.exit("ERROR: OPENAI_API_KEY not set")

    client = OpenAI(api_key=api_key)

    prompts = build_phase_prompts(data, selected)

    if os.environ.get("DEBUG_PROMPT"):
        for label, p in prompts:
            print(f"\n--- {label} PROMPT ---\n{p}\n--- END ---\n")

    print(f"\nSending {len(prompts)} parallel requests to {model}...")

    results = [None] * len(prompts)
    with concurrent.futures.ThreadPoolExecutor(max_workers=len(prompts)) as pool:
        future_to_idx = {
            pool.submit(call_one_phase, label, prompt, client, model): i
            for i, (label, prompt) in enumerate(prompts)
        }
        for future in concurrent.futures.as_completed(future_to_idx):
            i = future_to_idx[future]
            try:
                results[i] = future.result()
                print(f"  [{results[i][0]}] complete")
            except Exception as e:
                print(f"  [Phase {i+1}] ERROR: {e}")
                results[i] = (PHASE_LABELS[i], f"(request failed: {e})")

    print()
    for label, analysis in results:
        print("=" * 60)
        print(f"  {label.upper()}")
        print("=" * 60)
        print(analysis)
        print()


if __name__ == "__main__":
    main()
