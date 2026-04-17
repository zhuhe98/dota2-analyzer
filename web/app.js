const STATE = {
    data: null,
    activePlayerSlot: null,
};

// Dota 2 Engine Bounds (Approximated 7.33+ source 2 minimap sizes)
const MAP_BOUNDS = {
    minX: 8192,
    maxX: 24576,
    minY: 8192,
    maxY: 24576
};

async function init() {
    const overlay = document.getElementById('loading-overlay');
    overlay.classList.remove('hidden');

    try {
        const response = await fetch('output.json');
        STATE.data = await response.json();
        
        renderMeta();
        renderPlayerList();
    } catch (e) {
        document.getElementById('coach-text').innerText = "Failed to load output.json. Are you running through a web server? Error: " + e;
    } finally {
        overlay.classList.add('hidden');
    }
}

function renderMeta() {
    if (!STATE.data) return;
    const dur = Math.floor(STATE.data.duration_seconds / 60) + "m " + Math.round(STATE.data.duration_seconds % 60) + "s";
    
    document.getElementById('match-meta').innerHTML = `
        <strong>Match ID:</strong> ${STATE.data.match_id || 'N/A'}<br>
        <strong>Duration:</strong> ${dur}<br>
        <strong>Winner:</strong> <span class="highlight" style="color: var(--${STATE.data.winning_team})">${STATE.data.winning_team}</span>
    `;
}

function renderPlayerList() {
    const list = document.getElementById('player-list');
    list.innerHTML = '';

    STATE.data.players.forEach(p => {
        if (!p.hero_name) return;

        const div = document.createElement('div');
        div.className = 'player-item';
        div.onclick = () => selectPlayer(p.slot);
        
        div.innerHTML = `
            <div class="team-marker team-${p.team}"></div>
            <div class="player-info">
                <span class="hero-name">${p.hero_name.replace(/_/g, ' ').toUpperCase()}</span>
                <span class="player-name">${p.player_name || 'Anonymous'}</span>
            </div>
        `;
        div.dataset.slot = p.slot;
        list.appendChild(div);
    });
}

function selectPlayer(slot) {
    STATE.activePlayerSlot = slot;
    document.querySelectorAll('.player-item').forEach(el => {
        el.classList.toggle('active', parseInt(el.dataset.slot) === slot);
    });

    const player = STATE.data.players.find(p => p.slot === slot);
    if (!player) return;

    // Analyze Coach text
    analyzePlayer(player);

    // Render Heatmap
    if (player.positions && player.positions.length > 0) {
        renderHeatmap(player.positions, player.team === 'radiant' ? [34,197,94] : [239,68,68]);
    } else {
        clearCanvas();
        document.getElementById('coach-text').innerText += "\n\n(No position data available for this player. Did the parser run with the newest version?)";
    }
}

function analyzePlayer(player) {
    const text = document.getElementById('coach-text');
    let analysis = `<strong>Hero:</strong> ${player.hero_name}<br><strong>KDA:</strong> ${player.kills} / ${player.deaths} / ${player.assists}<br><br>`;

    if (player.deaths > 8) {
        analysis += "🔴 <strong>High Deaths:</strong> Your heatmap likely shows clustered 'danger zones'. Review position coordinates where you died heavily.\n<br><br>";
    }

    if (player.hero_damage < 15000 && player.slot < 5) {
        analysis += "🟠 <strong>Low Impact:</strong> Look at your heatmap to see if your trajectory is avoiding engagement zones (usually river and mid-lane).\n<br><br>";
    }

    if (player.net_worth > 15000) {
        analysis += "🟢 <strong>Good Farming:</strong> Your trajectory footprint should show efficient jungle-to-lane mapping.\n<br>";
    }

    text.innerHTML = analysis;
}

function clearCanvas() {
    const canvas = document.getElementById('heatmap-canvas');
    const ctx = canvas.getContext('2d');
    ctx.clearRect(0, 0, canvas.width, canvas.height);
}

function renderHeatmap(positions, rgb) {
    const canvas = document.getElementById('heatmap-canvas');
    const ctx = canvas.getContext('2d');
    
    // Clear previous
    ctx.clearRect(0, 0, canvas.width, canvas.height);

    const width = canvas.width;
    const height = canvas.height;

    // Offscreen canvas for mapping intensity
    const offCanvas = document.createElement('canvas');
    offCanvas.width = width;
    offCanvas.height = height;
    const offCtx = offCanvas.getContext('2d');

    const radius = 25;
    const intensity = 0.05; // Base opacity per overlapping point

    positions.forEach(pos => {
        // Map Engine X/Y to Canvas 0-1024
        // Engine Y is bottom-to-top, Canvas Y is top-to-bottom
        let normX = (pos.x - MAP_BOUNDS.minX) / (MAP_BOUNDS.maxX - MAP_BOUNDS.minX);
        let normY = (pos.y - MAP_BOUNDS.minY) / (MAP_BOUNDS.maxY - MAP_BOUNDS.minY);

        // Clamp
        normX = Math.max(0, Math.min(1, normX));
        normY = 1 - Math.max(0, Math.min(1, normY)); // Flip Y!

        const px = normX * width;
        const py = normY * height;

        const grad = offCtx.createRadialGradient(px, py, 0, px, py, radius);
        // Base color alpha logic: using black representing alpha coverage
        grad.addColorStop(0, `rgba(0, 0, 0, ${intensity})`);
        grad.addColorStop(1, 'rgba(0, 0, 0, 0)');

        offCtx.fillStyle = grad;
        offCtx.fillRect(px - radius, py - radius, radius * 2, radius * 2);
    });

    // Translate alpha density into colored intensity via ImageData
    const imgData = offCtx.getImageData(0, 0, width, height);
    const pd = imgData.data;
    
    // Colorize
    for (let i = 0; i < pd.length; i += 4) {
        const alpha = pd[i + 3];
        if (alpha > 0) {
            // Apply gradient mapping depending on density
            const val = alpha / 255;
            // Hotness
            let r = rgb[0], g = rgb[1], b = rgb[2];
            
            if (val > 0.5) { r = 255; g = 255; b = Math.floor(val*255); } 
            
            pd[i] = r;
            pd[i + 1] = g;
            pd[i + 2] = b;
            pd[i + 3] = alpha * 3; // Boost final opacity
        }
    }
    
    ctx.putImageData(imgData, 0, 0);
}

// Start
document.addEventListener('DOMContentLoaded', init);
