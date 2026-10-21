// SoundWave UI
// Calls the four microservices directly (CORS is enabled on all of them).
// Events are processed asynchronously, so the UI polls every 2 seconds and
// also refreshes about 1 second after each action.

const SERVICES = {
  playback: "http://localhost:8081",
  analytics: "http://localhost:8082",
  playlist: "http://localhost:8083",
  notification: "http://localhost:8084",
};

const POLL_MS = 2000;
const MILESTONE_EVERY = 5; // matches the Analytics default

// There is no Catalog service, so the track list is hard-coded here.
const TRACKS = [
  { track_id: "t1", artist_id: "a1", title: "Blinding Lights", artist: "The Weeknd", colors: ["#ff5f6d", "#7b2ff7"] },
  { track_id: "t2", artist_id: "a2", title: "Levitating", artist: "Dua Lipa", colors: ["#f857a6", "#ff9a44"] },
  { track_id: "t3", artist_id: "a3", title: "As It Was", artist: "Harry Styles", colors: ["#43c6ac", "#191654"] },
  { track_id: "t4", artist_id: "a4", title: "Shape of You", artist: "Ed Sheeran", colors: ["#2193b0", "#6dd5ed"] },
  { track_id: "t5", artist_id: "a5", title: "Bad Guy", artist: "Billie Eilish", colors: ["#a8e063", "#2f4f1f"] },
  { track_id: "t6", artist_id: "a6", title: "Heat Waves", artist: "Glass Animals", colors: ["#ffb347", "#c2185b"] },
  { track_id: "t7", artist_id: "a7", title: "Stay", artist: "The Kid LAROI", colors: ["#4776e6", "#8e54e9"] },
  { track_id: "t8", artist_id: "a8", title: "Flowers", artist: "Miley Cyrus", colors: ["#ee9ca7", "#5b3a70"] },
];

const PLAY_ICON =
  '<svg viewBox="0 0 24 24" fill="currentColor" aria-hidden="true"><path d="M7 4.5v15a1 1 0 0 0 1.5.86l12.5-7.5a1 1 0 0 0 0-1.72L8.5 3.64A1 1 0 0 0 7 4.5z"/></svg>';

// ---------- State ----------

const state = {
  user: "u1",
  view: "home",
  top: [],
  recent: [],
  playlists: [],
  notifications: [],
  selectedPlaylist: null,
  nowPlaying: null,
  // Used to detect what changed between polls, so we can show live activity.
  seen: { counts: null, recentKeys: null, notifIds: null, playlistIds: null },
};

// ---------- Helpers ----------

const $ = (sel) => document.querySelector(sel);

function trackById(id) {
  return TRACKS.find((t) => t.track_id === id);
}

function artStyle(trackId) {
  const t = trackById(trackId);
  const [a, b] = t ? t.colors : ["#2a3554", "#1e2842"];
  return `--a:${a};--b:${b}`;
}

function initials(title) {
  return title.split(/\s+/).map((w) => w[0]).join("").slice(0, 2).toUpperCase();
}

function escapeHtml(text) {
  const div = document.createElement("div");
  div.textContent = text ?? "";
  return div.innerHTML;
}

function timeAgo(iso) {
  const secs = Math.max(0, Math.round((Date.now() - new Date(iso).getTime()) / 1000));
  if (secs < 60) return `${secs}s ago`;
  if (secs < 3600) return `${Math.floor(secs / 60)}m ago`;
  if (secs < 86400) return `${Math.floor(secs / 3600)}h ago`;
  return new Date(iso).toLocaleDateString();
}

// Every request goes through here. 4xx and 5xx responses become errors
// carrying the service's own "error" text.
async function api(service, path, options = {}) {
  let res;
  try {
    res = await fetch(SERVICES[service] + path, {
      headers: { "Content-Type": "application/json" },
      ...options,
    });
  } catch {
    throw new Error(`The ${service} service is not reachable. Is it running?`);
  }
  const body = await res.json().catch(() => null);
  if (!res.ok) {
    throw new Error(body?.error || `${service} service returned ${res.status}`);
  }
  return body;
}

function toast(message, isError = false) {
  const el = document.createElement("div");
  el.className = "toast" + (isError ? " error" : "");
  el.textContent = message;
  $("#toasts").appendChild(el);
  setTimeout(() => el.remove(), isError ? 5000 : 3000);
}

function logActivity(html) {
  const list = $("#activity-list");
  const li = document.createElement("li");
  li.className = "fresh";
  li.innerHTML = `${html} <span class="notif-time">${new Date().toLocaleTimeString()}</span>`;
  list.prepend(li);
  while (list.children.length > 25) list.lastChild.remove();
}

// ---------- Actions ----------

async function playTrack(track, button) {
  if (button) button.disabled = true;
  try {
    await api("playback", "/plays", {
      method: "POST",
      body: JSON.stringify({
        user_id: state.user,
        track_id: track.track_id,
        artist_id: track.artist_id,
        title: track.title,
      }),
    });
    setNowPlaying(track);
    toast(`Playing ${track.title}`);
    logActivity(`Playback published <strong>track.played</strong> for ${escapeHtml(track.title)}`);
    setTimeout(refreshAll, 1000);
  } catch (err) {
    toast(err.message, true);
  } finally {
    if (button) button.disabled = false;
  }
}

async function createPlaylist(name) {
  try {
    const p = await api("playlist", "/playlists", {
      method: "POST",
      body: JSON.stringify({ user_id: state.user, name }),
    });
    toast(`Created playlist ${name}`);
    logActivity(`Playlist published <strong>playlist.created</strong> for ${escapeHtml(name)}`);
    state.selectedPlaylist = p.id;
    await refreshPlaylists();
    setTimeout(refreshAll, 1000);
    return true;
  } catch (err) {
    toast(err.message, true);
    return false;
  }
}

async function addTrackToPlaylist(playlist, track) {
  try {
    await api("playlist", `/playlists/${encodeURIComponent(playlist.id)}/tracks`, {
      method: "POST",
      body: JSON.stringify({ track_id: track.track_id, title: track.title, artist_id: track.artist_id }),
    });
    toast(`Added ${track.title} to ${playlist.name}`);
    if (state.selectedPlaylist === playlist.id) renderPlaylistDetail();
  } catch (err) {
    toast(err.message, true);
  }
}

// ---------- Rendering ----------

function renderTracks() {
  $("#track-grid").innerHTML = TRACKS.map(
    (t) => `
    <article class="track-card">
      <div class="art" style="${artStyle(t.track_id)}">
        ${initials(t.title)}
        <button class="play-btn" data-play="${t.track_id}" aria-label="Play ${escapeHtml(t.title)}">${PLAY_ICON}</button>
      </div>
      <p class="track-name">${escapeHtml(t.title)}</p>
      <p class="track-artist">${escapeHtml(t.artist)}</p>
      <button class="add-btn" data-add="${t.track_id}">Add to playlist</button>
    </article>`
  ).join("");
}

function trackRow(item, freshKeys = new Set()) {
  const t = trackById(item.track_id);
  const key = item.track_id + item.played_at;
  const right = item.played_at
    ? `<span class="row-time">${timeAgo(item.played_at)}</span>`
    : `<button class="play-btn" data-play="${item.track_id}" aria-label="Play ${escapeHtml(item.title)}" style="width:36px;height:36px">${PLAY_ICON}</button>`;
  return `
    <li class="${freshKeys.has(key) ? "fresh" : ""}">
      <div class="mini-art" style="${artStyle(item.track_id)}"></div>
      <div>
        <p class="row-title">${escapeHtml(item.title)}</p>
        <p class="row-sub">${escapeHtml(t ? t.artist : item.artist_id || "")}</p>
      </div>
      ${right}
    </li>`;
}

function renderRecent(freshKeys) {
  const list = $("#recent-list");
  if (!state.recent.length) {
    list.innerHTML = `<li class="empty">Nothing played yet. Press play on any track above.</li>`;
    return;
  }
  list.innerHTML = state.recent.map((r) => trackRow(r, freshKeys)).join("");
}

function renderCharts(changedIds = new Set()) {
  const list = $("#chart-list");
  if (!state.top.length) {
    list.innerHTML = `<li class="empty">No plays counted yet. Play a track from Home to start the charts.</li>`;
    return;
  }
  list.innerHTML = state.top
    .map((s, i) => {
      const toNext = s.play_count % MILESTONE_EVERY;
      const pct = toNext === 0 ? 100 : (toNext / MILESTONE_EVERY) * 100;
      const t = trackById(s.track_id);
      return `
      <li class="${changedIds.has(s.track_id) ? "fresh" : ""}">
        <span class="rank">${i + 1}</span>
        <div class="mini-art" style="${artStyle(s.track_id)}"></div>
        <div>
          <p class="row-title">${escapeHtml(s.title)}</p>
          <p class="row-sub">${escapeHtml(t ? t.artist : s.artist_id)}</p>
        </div>
        <div class="plays">
          <span class="plays-count">${s.play_count} <small>${s.play_count === 1 ? "play" : "plays"}</small></span>
          <div class="meter" title="Progress to the next milestone"><span style="width:${pct}%"></span></div>
        </div>
      </li>`;
    })
    .join("");
}

function renderPlaylists() {
  const list = $("#playlist-list");
  if (!state.playlists.length) {
    list.innerHTML = `<li class="empty">No playlists yet. Name one above to create it.</li>`;
    $("#playlist-detail").innerHTML = `<p class="detail-sub">Your playlist's tracks will show here.</p>`;
    return;
  }
  list.innerHTML = state.playlists
    .map(
      (p) => `
      <li>
        <button class="playlist-item ${p.id === state.selectedPlaylist ? "active" : ""}" data-playlist="${p.id}">
          <div class="mini-art" style="--a:#ffb547;--b:#4fd1c5"></div>
          <span class="row-title">${escapeHtml(p.name)}</span>
        </button>
      </li>`
    )
    .join("");
}

async function renderPlaylistDetail() {
  const box = $("#playlist-detail");
  if (!state.selectedPlaylist) {
    box.innerHTML = `<p class="detail-sub">Select a playlist to see its tracks.</p>`;
    return;
  }
  try {
    const p = await api("playlist", `/playlists/${encodeURIComponent(state.selectedPlaylist)}`);
    const tracks = p.tracks || [];
    box.innerHTML = `
      <h2 class="detail-title">${escapeHtml(p.name)}</h2>
      <p class="detail-sub">${tracks.length} ${tracks.length === 1 ? "track" : "tracks"}</p>
      ${
        tracks.length
          ? `<ol class="track-list">${tracks.map((t) => trackRow(t)).join("")}</ol>`
          : `<p class="empty">This playlist is empty. Use "Add to playlist" on any track on Home.</p>`
      }`;
  } catch (err) {
    box.innerHTML = `<p class="empty">${escapeHtml(err.message)}</p>`;
  }
}

function renderNotifications(freshIds = new Set()) {
  const list = $("#notif-list");
  $("#notif-count").textContent = state.notifications.length;
  if (!state.notifications.length) {
    list.innerHTML = `<li class="empty" style="border-left:1px dashed var(--line)">No notifications yet. Create a playlist or play a track 5 times.</li>`;
    return;
  }
  list.innerHTML = state.notifications
    .map(
      (n) => `
      <li class="${n.user_id === "all" ? "broadcast" : ""} ${freshIds.has(n.id) ? "fresh" : ""}">
        ${escapeHtml(n.message)}
        <span class="notif-time">${timeAgo(n.created_at)}</span>
      </li>`
    )
    .join("");
}

function setNowPlaying(track) {
  state.nowPlaying = track;
  $("#player").classList.add("playing");
  $("#player-art").setAttribute("style", artStyle(track.track_id));
  // Tint the blurred background glow with this track's colours
  document.body.style.setProperty("--glow-a", track.colors[0]);
  document.body.style.setProperty("--glow-b", track.colors[1]);
  $("#player-title").textContent = track.title;
  $("#player-artist").textContent = track.artist;
  updatePlayerCount();
}

function updatePlayerCount() {
  if (!state.nowPlaying) return;
  const stat = state.top.find((s) => s.track_id === state.nowPlaying.track_id);
  $("#player-plays").textContent = stat ? `${stat.play_count} total plays` : "Counting play...";
}

function renderGreeting() {
  const h = new Date().getHours();
  $("#greeting").textContent = h < 12 ? "Good morning" : h < 18 ? "Good afternoon" : "Good evening";
}

// ---------- Data refresh (polling) ----------

async function refreshHealth() {
  const items = document.querySelectorAll("#service-list li");
  await Promise.all(
    [...items].map(async (li) => {
      try {
        const res = await fetch(`http://localhost:${li.dataset.port}/health`);
        li.className = res.ok ? "up" : "down";
      } catch {
        li.className = "down";
      }
    })
  );
}

async function refreshTop() {
  try {
    state.top = (await api("analytics", "/stats/top")) || [];
  } catch {
    return; // service down: keep the last data, the status dot shows the problem
  }
  const counts = Object.fromEntries(state.top.map((s) => [s.track_id, s.play_count]));
  const changed = new Set();
  if (state.seen.counts) {
    for (const s of state.top) {
      const before = state.seen.counts[s.track_id] || 0;
      if (s.play_count > before) {
        changed.add(s.track_id);
        logActivity(`Analytics counted <strong>${escapeHtml(s.title)}</strong>: ${s.play_count} ${s.play_count === 1 ? "play" : "plays"}`);
      }
    }
  }
  state.seen.counts = counts;
  renderCharts(changed);
  updatePlayerCount();
}

async function refreshRecent() {
  try {
    state.recent = (await api("playlist", `/users/${encodeURIComponent(state.user)}/recent`)) || [];
  } catch {
    return;
  }
  const keys = new Set(state.recent.map((r) => r.track_id + r.played_at));
  const fresh = new Set();
  if (state.seen.recentKeys) {
    for (const r of state.recent) {
      const key = r.track_id + r.played_at;
      if (!state.seen.recentKeys.has(key)) {
        fresh.add(key);
        logActivity(`Playlist added <strong>${escapeHtml(r.title)}</strong> to recently played`);
      }
    }
  }
  state.seen.recentKeys = keys;
  renderRecent(fresh);
}

async function refreshPlaylists() {
  try {
    state.playlists = (await api("playlist", `/playlists?user_id=${encodeURIComponent(state.user)}`)) || [];
  } catch {
    return;
  }
  const ids = new Set(state.playlists.map((p) => p.id));
  if (!state.selectedPlaylist && state.playlists.length) state.selectedPlaylist = state.playlists[0].id;
  if (state.selectedPlaylist && !ids.has(state.selectedPlaylist)) state.selectedPlaylist = null;
  state.seen.playlistIds = ids;
  renderPlaylists();
  if (state.view === "playlists") renderPlaylistDetail();
}

async function refreshNotifications() {
  try {
    state.notifications = (await api("notification", `/notifications?user_id=${encodeURIComponent(state.user)}`)) || [];
  } catch {
    return;
  }
  const fresh = new Set();
  if (state.seen.notifIds) {
    for (const n of state.notifications) {
      if (!state.seen.notifIds.has(n.id)) {
        fresh.add(n.id);
        logActivity(`Notification received: <strong>${escapeHtml(n.message)}</strong>`);
        if (n.type && n.type.includes("milestone")) toast(n.message);
      }
    }
  }
  state.seen.notifIds = new Set(state.notifications.map((n) => n.id));
  renderNotifications(fresh);
}

function refreshAll() {
  return Promise.all([refreshHealth(), refreshTop(), refreshRecent(), refreshPlaylists(), refreshNotifications()]);
}

// ---------- Navigation and events ----------

function showView(view) {
  state.view = view;
  document.querySelectorAll(".view").forEach((v) => v.classList.toggle("active", v.id === `view-${view}`));
  document.querySelectorAll(".nav-item").forEach((b) => b.classList.toggle("active", b.dataset.view === view));
  if (view === "playlists") renderPlaylistDetail();
}

function openAddDialog(track) {
  if (!state.playlists.length) {
    toast("Create a playlist first in Your playlists", true);
    showView("playlists");
    $("#playlist-name").focus();
    return;
  }
  const dialog = $("#add-dialog");
  $("#add-dialog-track").textContent = `${track.title} by ${track.artist}`;
  $("#add-dialog-list").innerHTML = state.playlists
    .map(
      (p) => `
      <li>
        <button class="playlist-item" value="${p.id}">
          <div class="mini-art" style="--a:#ffb547;--b:#4fd1c5"></div>
          <span class="row-title">${escapeHtml(p.name)}</span>
        </button>
      </li>`
    )
    .join("");
  dialog.onclose = () => {
    const playlist = state.playlists.find((p) => p.id === dialog.returnValue);
    if (playlist) addTrackToPlaylist(playlist, track);
  };
  dialog.returnValue = "";
  dialog.showModal();
}

function bindEvents() {
  document.querySelectorAll(".nav-item").forEach((btn) => btn.addEventListener("click", () => showView(btn.dataset.view)));

  // One click handler for all play, add and playlist buttons (they are re-rendered often).
  document.addEventListener("click", (e) => {
    const play = e.target.closest("[data-play]");
    if (play) return playTrack(trackById(play.dataset.play), play);

    const add = e.target.closest("[data-add]");
    if (add) return openAddDialog(trackById(add.dataset.add));

    const pl = e.target.closest("[data-playlist]");
    if (pl) {
      state.selectedPlaylist = pl.dataset.playlist;
      renderPlaylists();
      renderPlaylistDetail();
    }
  });

  $("#create-playlist-form").addEventListener("submit", async (e) => {
    e.preventDefault();
    const input = $("#playlist-name");
    const name = input.value.trim();
    if (!name) return;
    if (await createPlaylist(name)) input.value = "";
  });

  // Switching user resets the per-user data so the activity feed does not report old items as new.
  $("#user-select").addEventListener("change", (e) => {
    state.user = e.target.value;
    state.selectedPlaylist = null;
    state.seen.recentKeys = null;
    state.seen.notifIds = null;
    state.seen.playlistIds = null;
    logActivity(`Switched to listener <strong>${escapeHtml(state.user)}</strong>`);
    refreshAll();
  });
}

// ---------- Start ----------

renderGreeting();
renderTracks();
bindEvents();
refreshAll();
setInterval(refreshAll, POLL_MS);
