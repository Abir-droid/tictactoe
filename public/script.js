let ws;
let myUsername = "";
let mySymbol = "";
let currentTurn = "";
let opponentUsername = "";
let p1SessionScore = 0;
let p2SessionScore = 0;
let isChatOpen = false;
let popupTimer = null;

const svgIconX = `<svg viewBox="0 0 24 24"><line x1="18" y1="6" x2="6" y2="18"></line><line x1="6" y1="6" x2="18" y2="18"></line></svg>`;
const svgIconO = `<svg viewBox="0 0 24 24"><circle cx="12" cy="12" r="8"></circle></svg>`;

const winningCombos = [
  [0, 1, 2], [3, 4, 5], [6, 7, 8],
  [0, 3, 6], [1, 4, 7], [2, 5, 8],
  [0, 4, 8], [2, 4, 6]
];

const comboLineCoords = [
  { x1: "10%", y1: "16.6%", x2: "90%", y2: "16.6%" },
  { x1: "10%", y1: "50%",   x2: "90%", y2: "50%" },
  { x1: "10%", y1: "83.3%", x2: "90%", y2: "83.3%" },
  { x1: "16.6%", y1: "10%", x2: "16.6%", y2: "90%" },
  { x1: "50%",   y1: "10%", x2: "50%",   y2: "90%" },
  { x1: "83.3%", y1: "10%", x2: "83.3%", y2: "90%" },
  { x1: "10%",   y1: "10%", x2: "90%",   y2: "90%" },
  { x1: "90%",   y1: "10%", x2: "10%",   y2: "90%" }
];

window.addEventListener("DOMContentLoaded", () => {
  initDraggableChat();

  const savedTheme = localStorage.getItem("cyber_game_theme") || "dark";
  applyTheme(savedTheme);

  const savedUser = localStorage.getItem("cyber_game_user");
  if (savedUser) {
    myUsername = savedUser;
    openDashboard();
  }
});

function applyTheme(theme) {
  const html = document.documentElement;
  html.setAttribute("data-theme", theme);
  if (theme === "light") {
    document.getElementById("icon-sun").classList.add("hidden");
    document.getElementById("icon-moon").classList.remove("hidden");
  } else {
    document.getElementById("icon-moon").classList.add("hidden");
    document.getElementById("icon-sun").classList.remove("hidden");
  }
}

function toggleTheme() {
  const html = document.documentElement;
  const current = html.getAttribute("data-theme");
  const next = current === "dark" ? "light" : "dark";
  localStorage.setItem("cyber_game_theme", next);
  applyTheme(next);
}

function showView(viewId) {
  document.getElementById("login-view").classList.add("hidden");
  document.getElementById("register-view").classList.add("hidden");
  document.getElementById("dashboard-view").classList.add("hidden");
  document.getElementById("game-view").classList.add("hidden");
  document.getElementById("scoreboard-card").classList.add("hidden");
  document.getElementById("chat-card").classList.add("hidden");
  document.getElementById("chat-toggle-btn").classList.add("hidden");
  document.getElementById("top-left-nav").classList.add("hidden");
  document.getElementById("chat-popup").classList.add("hidden");

  if (viewId === "game-view") {
    document.getElementById("game-view").classList.remove("hidden");
    document.getElementById("top-left-nav").classList.remove("hidden");
  } else {
    document.getElementById(viewId).classList.remove("hidden");
  }
}

async function login() {
  const user = document.getElementById("login-username").value.trim();
  const pass = document.getElementById("login-password").value.trim();
  if (!user || !pass) return alert("Enter both username and password");

  const res = await fetch("/api/login", {
    method: "POST",
    headers: { "Content-Type": "application/json" },
    body: JSON.stringify({ username: user, password: pass })
  });

  if (res.ok) {
    myUsername = user;
    localStorage.setItem("cyber_game_user", user);
    openDashboard();
  } else {
    const err = await res.text();
    alert(err);
  }
}

async function register() {
  const user = document.getElementById("reg-username").value.trim();
  const pass = document.getElementById("reg-password").value.trim();
  if (!user || !pass) return alert("Enter both username and password");

  const res = await fetch("/api/register", {
    method: "POST",
    headers: { "Content-Type": "application/json" },
    body: JSON.stringify({ username: user, password: pass })
  });

  if (res.ok) {
    myUsername = user;
    localStorage.setItem("cyber_game_user", user);
    openDashboard();
  } else {
    const err = await res.text();
    alert(err);
  }
}

function openDashboard() {
  document.getElementById("user-display").innerText = myUsername;
  showView("dashboard-view");
  loadLeaderboard();
  loadProfileStats();
  connectWebSocket();
}

async function loadProfileStats() {
  try {
    const res = await fetch(`/api/profile?username=${encodeURIComponent(myUsername)}`);
    if (res.ok) {
      const stats = await res.json();
      document.getElementById("prof-wins").innerText = stats.wins;
      document.getElementById("prof-losses").innerText = stats.losses;
      document.getElementById("prof-draws").innerText = stats.draws;
      document.getElementById("prof-total").innerText = stats.total;
    }
  } catch (e) {
    console.error("Failed to load profile stats", e);
  }
}

async function loadLeaderboard() {
  const tbody = document.getElementById("leaderboard-body");
  tbody.innerHTML = '<tr><td colspan="5">Loading...</td></tr>';

  try {
    const res = await fetch("/api/leaderboard");
    const data = await res.json();

    if (data.length === 0) {
      tbody.innerHTML = '<tr><td colspan="5">No games played yet</td></tr>';
      return;
    }

    tbody.innerHTML = data.map(player => `
      <tr>
        <td><strong>${player.username}</strong></td>
        <td style="color: var(--accent-green);">${player.wins}</td>
        <td style="color: #ef4444;">${player.losses}</td>
        <td style="color: var(--accent-amber);">${player.draws}</td>
        <td style="color: var(--accent-blue); font-weight: 700;">${player.total !== undefined ? player.total : (player.wins + player.losses + player.draws)}</td>
      </tr>
    `).join('');
  } catch (e) {
    tbody.innerHTML = '<tr><td colspan="5">Failed to load</td></tr>';
  }
}

function connectWebSocket() {
  if (ws && ws.readyState === WebSocket.OPEN) return;

  const protocol = location.protocol === 'https:' ? 'wss:' : 'ws:';
  ws = new WebSocket(`${protocol}//${location.host}/ws?username=${myUsername}`);

  ws.onmessage = (event) => {
    const msg = JSON.parse(event.data);

    switch (msg.type) {
      case "ACTIVE_GAME_AVAILABLE":
        document.getElementById("rejoin-opp-name").innerText = msg.opponent;
        document.getElementById("rejoin-banner").classList.remove("hidden");
        const gameView = document.getElementById("game-view");
        if (gameView && !gameView.classList.contains("hidden")) {
          rejoinOngoingGame();
        }
        break;

      case "MATCH_FOUND":
        p1SessionScore = 0;
        p2SessionScore = 0;
        document.getElementById("rejoin-banner").classList.add("hidden");
        setupMatch(msg);
        break;

      case "REJOIN_STATE":
        document.getElementById("rejoin-banner").classList.add("hidden");
        setupRejoinMatch(msg);
        break;

      case "GAME_STATE":
        currentTurn = msg.turn;
        renderBoard(msg.board);
        updateStatus(`Turn: Player [${currentTurn}]`);
        updateScoreboard();
        break;

      case "GAME_OVER":
        renderBoard(msg.board);
        checkAndDrawWinLine(msg.board, msg.winner);
        if (msg.winner === "DRAW") {
          updateStatus("Game Over: It's a Draw!");
        } else if (msg.winner === mySymbol) {
          p1SessionScore++;
          updateStatus("🎉 Victory!");
          triggerFireworks();
        } else {
          p2SessionScore++;
          updateStatus("❌ Defeat!");
        }
        currentTurn = "";
        updateScoreboard();
        document.getElementById("retry-btn").classList.remove("hidden");
        document.getElementById("retry-btn").disabled = false;
        document.getElementById("new-opp-btn").classList.remove("hidden");
        break;

      case "RETRY_REQUESTED":
        document.getElementById("retry-msg").innerText = msg.message;
        break;

      case "RESTART_GAME":
        clearWinningLine();
        document.getElementById("retry-btn").classList.add("hidden");
        document.getElementById("new-opp-btn").classList.add("hidden");
        document.getElementById("retry-msg").innerText = "";
        mySymbol = msg.symbol;
        currentTurn = msg.turn;
        updateScoreboard();
        renderBoard(msg.board);
        updateStatus(`Rematch Started! Turn: Player [${currentTurn}]`);
        break;

      case "CHAT":
        appendChatMessage(msg.sender, msg.message);
        break;

      case "OPPONENT_DISCONNECTED":
        updateStatus(`⚠️ ${msg.message}`);
        break;

      case "OPPONENT_RECONNECTED":
        updateStatus(`🟢 ${msg.message}`);
        break;

      case "OPPONENT_LEFT":
        updateStatus(msg.message);
        clearWinningLine();
        document.getElementById("board").classList.add("hidden");
        document.getElementById("scoreboard-card").classList.add("hidden");
        document.getElementById("chat-card").classList.add("hidden");
        document.getElementById("chat-toggle-btn").classList.add("hidden");
        document.getElementById("retry-btn").classList.add("hidden");
        document.getElementById("new-opp-btn").classList.add("hidden");
        document.getElementById("search-spinner").classList.add("hidden");
        document.getElementById("retry-msg").innerText = "";
        document.getElementById("rejoin-banner").classList.add("hidden");
        break;

      case "ERROR":
        alert(msg.error);
        break;
    }
  };

  ws.onclose = () => {
    setTimeout(() => {
      if (myUsername) connectWebSocket();
    }, 3000);
  };
}

function rejoinOngoingGame() {
  if (ws && ws.readyState === WebSocket.OPEN) {
    ws.send(JSON.stringify({ type: "REJOIN_GAME" }));
  }
}

function startNewGame() {
  showView("game-view");
  resetGameUI();
  findMatch();
}

function returnToDashboard() {
  if (ws && ws.readyState === WebSocket.OPEN) {
    ws.send(JSON.stringify({ type: "LEAVE_QUEUE" }));
  }
  showView("dashboard-view");
  loadLeaderboard();
  loadProfileStats();
  resetGameUI();
}

function logout() {
  if (ws) {
    ws.onclose = null;
    ws.close();
  }
  myUsername = "";
  localStorage.removeItem("cyber_game_user");
  document.getElementById("login-username").value = "";
  document.getElementById("login-password").value = "";
  document.getElementById("reg-username").value = "";
  document.getElementById("reg-password").value = "";
  showView("login-view");
}

function resetGameUI() {
  clearWinningLine();
  document.getElementById("board").classList.add("hidden");
  document.getElementById("scoreboard-card").classList.add("hidden");
  document.getElementById("chat-card").classList.add("hidden");
  document.getElementById("chat-toggle-btn").classList.add("hidden");
  document.getElementById("retry-btn").classList.add("hidden");
  document.getElementById("new-opp-btn").classList.add("hidden");
  document.getElementById("search-spinner").classList.add("hidden");
  document.getElementById("retry-msg").innerText = "";
  document.getElementById("chat-messages").innerHTML = "";
  isChatOpen = false;
  updateStatus("Searching for another player...");
}

function setupMatch(msg) {
  mySymbol = msg.symbol;
  currentTurn = msg.turn;
  opponentUsername = msg.opponent;

  document.getElementById("search-spinner").classList.add("hidden");
  document.getElementById("retry-btn").classList.add("hidden");
  document.getElementById("new-opp-btn").classList.add("hidden");
  document.getElementById("board").classList.remove("hidden");
  document.getElementById("scoreboard-card").classList.remove("hidden");
  document.getElementById("chat-toggle-btn").classList.remove("hidden");
  document.getElementById("retry-msg").innerText = "";
  document.getElementById("chat-messages").innerHTML = "";

  clearWinningLine();
  updateScoreboard();
  renderBoard(["", "", "", "", "", "", "", "", ""]);
  updateStatus(`Matched vs ${msg.opponent}! Turn: Player [${currentTurn}]`);
}

function setupRejoinMatch(msg) {
  mySymbol = msg.symbol;
  currentTurn = msg.turn;
  opponentUsername = msg.opponent;

  showView("game-view");
  document.getElementById("search-spinner").classList.add("hidden");
  document.getElementById("retry-btn").classList.add("hidden");
  document.getElementById("new-opp-btn").classList.add("hidden");
  document.getElementById("board").classList.remove("hidden");
  document.getElementById("scoreboard-card").classList.remove("hidden");
  document.getElementById("chat-toggle-btn").classList.remove("hidden");

  clearWinningLine();
  updateScoreboard();
  renderBoard(msg.board);
  updateStatus(`Rejoined vs ${msg.opponent}! Turn: Player [${currentTurn}]`);
}

function updateScoreboard() {
  document.getElementById("sb-p1-name").innerText = myUsername;
  document.getElementById("sb-p1-sym").innerText = `[${mySymbol}]`;
  document.getElementById("sb-p1-sym").className = `player-tag ${mySymbol === 'X' ? 'tag-x' : 'tag-o'}`;
  document.getElementById("sb-p1-score").innerText = p1SessionScore;

  const oppSymbol = mySymbol === 'X' ? 'O' : 'X';
  document.getElementById("sb-p2-name").innerText = opponentUsername;
  document.getElementById("sb-p2-sym").innerText = `[${oppSymbol}]`;
  document.getElementById("sb-p2-sym").className = `player-tag ${oppSymbol === 'X' ? 'tag-x' : 'tag-o'}`;
  document.getElementById("sb-p2-score").innerText = p2SessionScore;

  const p1Avatar = document.getElementById("sb-p1-avatar");
  const p2Avatar = document.getElementById("sb-p2-avatar");
  p1Avatar.classList.remove("active-turn");
  p2Avatar.classList.remove("active-turn");

  if (currentTurn === mySymbol) {
    p1Avatar.classList.add("active-turn");
  } else if (currentTurn === oppSymbol) {
    p2Avatar.classList.add("active-turn");
  }
}

function toggleChatDrawer(forcedState) {
  if (typeof forcedState === 'boolean') {
    isChatOpen = forcedState;
  } else {
    isChatOpen = !isChatOpen;
  }

  const chatCard = document.getElementById("chat-card");
  if (isChatOpen) {
    chatCard.classList.remove("hidden");
    document.getElementById("chat-popup").classList.add("hidden");
  } else {
    chatCard.classList.add("hidden");
  }
}

function sendChat() {
  const input = document.getElementById("chat-input");
  const text = input.value.trim();
  if (!text || !ws) return;

  ws.send(JSON.stringify({ type: "CHAT", message: text }));
  input.value = "";
}

function quickSendEmoji(emoji) {
  if (!ws) return;
  ws.send(JSON.stringify({ type: "CHAT", message: emoji }));
}

function appendChatMessage(sender, text) {
  const container = document.getElementById("chat-messages");
  const isMe = sender === myUsername;

  const msgDiv = document.createElement("div");
  msgDiv.className = `chat-msg ${isMe ? "me" : "them"}`;
  msgDiv.innerText = isMe ? text : `${sender}: ${text}`;

  container.appendChild(msgDiv);
  container.scrollTop = container.scrollHeight;

  if (!isMe && !isChatOpen) {
    showChatPopup(sender, text);
  }
}

function showChatPopup(sender, text) {
  const popup = document.getElementById("chat-popup");
  document.getElementById("popup-sender").innerText = sender;
  document.getElementById("popup-text").innerText = text;

  popup.classList.remove("hidden");

  if (popupTimer) clearTimeout(popupTimer);
  popupTimer = setTimeout(() => {
    popup.classList.add("hidden");
  }, 4000);
}

function findMatch() {
  ws.send(JSON.stringify({ type: "JOIN_QUEUE" }));
  document.getElementById("search-spinner").classList.remove("hidden");
  document.getElementById("scoreboard-card").classList.add("hidden");
  document.getElementById("chat-card").classList.add("hidden");
  document.getElementById("chat-toggle-btn").classList.add("hidden");
  document.getElementById("retry-btn").classList.add("hidden");
  document.getElementById("new-opp-btn").classList.add("hidden");
  document.getElementById("board").classList.add("hidden");
  document.getElementById("retry-msg").innerText = "";
  updateStatus("Searching for another player...");
}

function requestRetry() {
  ws.send(JSON.stringify({ type: "RETRY" }));
  document.getElementById("retry-btn").disabled = true;
  updateStatus("Waiting for opponent to accept rematch...");
}

function makeMove(position) {
  if (currentTurn !== mySymbol) return;
  ws.send(JSON.stringify({ type: "MOVE", position }));
}

function renderBoard(board) {
  const cells = document.querySelectorAll(".cell");
  board.forEach((val, i) => {
    const cell = cells[i];
    const hasX = cell.classList.contains("pop-x");
    const hasO = cell.classList.contains("pop-o");

    if (val === "X") {
      if (!hasX) {
        cell.innerHTML = svgIconX;
        cell.className = "cell pop-x";
      }
    } else if (val === "O") {
      if (!hasO) {
        cell.innerHTML = svgIconO;
        cell.className = "cell pop-o";
      }
    } else {
      cell.innerHTML = "";
      cell.className = "cell";
    }
  });
}

function checkAndDrawWinLine(board, winner) {
  if (winner !== "X" && winner !== "O") return;

  winningCombos.forEach((combo, idx) => {
    if (board[combo[0]] === winner && board[combo[1]] === winner && board[combo[2]] === winner) {
      const coords = comboLineCoords[idx];
      const svg = document.getElementById("win-line-svg");
      const line = document.getElementById("win-line");

      line.setAttribute("x1", coords.x1);
      line.setAttribute("y1", coords.y1);
      line.setAttribute("x2", coords.x2);
      line.setAttribute("y2", coords.y2);

      line.className = `win-line ${winner === "O" ? "line-o" : ""}`;
      svg.classList.remove("hidden");
    }
  });
}

function clearWinningLine() {
  const svg = document.getElementById("win-line-svg");
  if (svg) svg.classList.add("hidden");
}

function updateStatus(txt) {
  document.getElementById("status").innerText = txt;
}

function initDraggableChat() {
  const el = document.getElementById("chat-card");
  const handle = document.getElementById("chat-header");
  let p1 = 0, p2 = 0, p3 = 0, p4 = 0;

  handle.onmousedown = (e) => {
    e.preventDefault();
    p3 = e.clientX;
    p4 = e.clientY;
    document.onmouseup = () => { document.onmouseup = null; document.onmousemove = null; };
    document.onmousemove = (e) => {
      e.preventDefault();
      p1 = p3 - e.clientX;
      p2 = p4 - e.clientY;
      p3 = e.clientX;
      p4 = e.clientY;
      el.style.top = (el.offsetTop - p2) + "px";
      el.style.left = (el.offsetLeft - p1) + "px";
    };
  };

  handle.ontouchstart = (e) => {
    const touch = e.touches[0];
    p3 = touch.clientX;
    p4 = touch.clientY;
    document.ontouchend = () => { document.ontouchend = null; document.ontouchmove = null; };
    document.ontouchmove = (e) => {
      const touch = e.touches[0];
      p1 = p3 - touch.clientX;
      p2 = p4 - touch.clientY;
      p3 = touch.clientX;
      p4 = touch.clientY;
      el.style.top = (el.offsetTop - p2) + "px";
      el.style.left = (el.offsetLeft - p1) + "px";
    };
  };
}

function triggerFireworks() {
  const canvas = document.getElementById("confetti-canvas");
  const ctx = canvas.getContext("2d");
  canvas.width = window.innerWidth;
  canvas.height = window.innerHeight;

  const particles = [];
  const colors = ['#38bdf8', '#06b6d4', '#ec4899', '#10b981', '#f59e0b', '#ffffff'];

  for (let i = 0; i < 120; i++) {
    particles.push({
      x: canvas.width / 2,
      y: canvas.height / 2,
      vx: (Math.random() - 0.5) * 14,
      vy: (Math.random() - 0.7) * 16,
      size: Math.random() * 8 + 4,
      color: colors[Math.floor(Math.random() * colors.length)],
      alpha: 1,
      decay: Math.random() * 0.015 + 0.008
    });
  }

  function render() {
    ctx.clearRect(0, 0, canvas.width, canvas.height);
    let active = false;

    particles.forEach(p => {
      if (p.alpha > 0) {
        active = true;
        p.x += p.vx;
        p.y += p.vy;
        p.vy += 0.25;
        p.alpha -= p.decay;

        ctx.globalAlpha = Math.max(p.alpha, 0);
        ctx.fillStyle = p.color;
        ctx.fillRect(p.x, p.y, p.size, p.size);
      }
    });

    if (active) {
      requestAnimationFrame(render);
    } else {
      ctx.clearRect(0, 0, canvas.width, canvas.height);
    }
  }

  render();
}