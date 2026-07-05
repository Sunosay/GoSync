/* GoSync 前端：调用本地 Go 后端提供的 API，并通过 SSE 接收实时进度。 */
(() => {
  const $ = (id) => document.getElementById(id);
  const fmtBytes = (n) => {
    if (!n) return "0 B";
    const k = 1024;
    if (n < k) return n + " B";
    if (n < k * k) return (n / k).toFixed(1) + " KB";
    if (n < k * k * k) return (n / k / k).toFixed(1) + " MB";
    return (n / k / k / k).toFixed(2) + " GB";
  };
  const fmtTime = (sec) => {
    if (!sec) return "—";
    const d = new Date(sec * 1000);
    const pad = (n) => String(n).padStart(2, "0");
    return d.getFullYear() + "-" + pad(d.getMonth() + 1) + "-" + pad(d.getDate()) +
      " " + pad(d.getHours()) + ":" + pad(d.getMinutes()) + ":" + pad(d.getSeconds());
  };
  const fmtSpeed = (bps) => {
    if (!bps || bps <= 0) return "—";
    return fmtBytes(bps) + "/s";
  };

  let deviceId = "";
  let activeSync = null;
  let sseSource = null;

  function toast(msg, kind = "info", ms = 3000) {
    const el = $("toast");
    el.textContent = msg;
    el.className = "toast " + kind;
    el.hidden = false;
    clearTimeout(toast._t);
    toast._t = setTimeout(() => (el.hidden = true), ms);
  }

  async function api(method, path, body) {
    const opt = { method, headers: {} };
    if (body !== undefined) {
      opt.headers["Content-Type"] = "application/json";
      opt.body = JSON.stringify(body);
    }
    const r = await fetch(path, opt);
    if (!r.ok) {
      const t = await r.text();
      throw new Error(t || r.statusText);
    }
    const ct = r.headers.get("Content-Type") || "";
    if (ct.includes("application/json")) return r.json();
    return r.text();
  }

  // === 设备信息 ===
  async function loadInfo() {
    const info = await api("GET", "/api/info");
    deviceId = info.device_id;
    $("deviceId").textContent = info.device_id;
    $("deviceListen").textContent = info.listen;
  }

  $("copyIdBtn").addEventListener("click", () => {
    if (!deviceId) return;
    navigator.clipboard.writeText(deviceId).then(
      () => toast("已复制本机 ID", "success"),
      () => toast("复制失败", "error"),
    );
  });

  // === 文件夹 ===
  async function loadFolders() {
    const list = await api("GET", "/api/folders");
    const ul = $("folderList");
    ul.innerHTML = "";
    if (!list.length) {
      ul.innerHTML = '<li class="empty">尚未添加同步文件夹</li>';
      return;
    }
    for (const f of list) {
      const li = document.createElement("li");
      li.dataset.path = f.path;
      const meta = document.createElement("div");
      meta.className = "folder-meta";
      const p = document.createElement("div");
      p.className = "folder-path";
      p.textContent = f.path;
      meta.appendChild(p);
      const s = document.createElement("div");
      s.className = "folder-stats";
      if (f.exists) {
        s.textContent = f.num_files + " 个文件 · " + fmtBytes(f.total_size);
      } else {
        s.innerHTML = '<span class="pill fail">不可访问</span> ' + (f.error || "");
      }
      meta.appendChild(s);
      li.appendChild(meta);
      const actions = document.createElement("div");
      actions.className = "folder-actions";
      const viewBtn = document.createElement("button");
      viewBtn.className = "btn-ghost";
      viewBtn.textContent = "查看";
      viewBtn.onclick = () => openFolder(f.path);
      const syncBtn = document.createElement("button");
      syncBtn.className = "btn-ghost";
      syncBtn.textContent = "同步此文件夹";
      syncBtn.disabled = !f.exists;
      syncBtn.onclick = () => promptSync(f.path);
      const delBtn = document.createElement("button");
      delBtn.className = "btn-ghost btn-danger";
      delBtn.textContent = "移除";
      delBtn.onclick = () => removeFolder(f.path);
      actions.appendChild(viewBtn);
      actions.appendChild(syncBtn);
      actions.appendChild(delBtn);
      li.appendChild(actions);
      ul.appendChild(li);
    }
  }

  $("addFolderForm").addEventListener("submit", async (e) => {
    e.preventDefault();
    const path = $("folderPathInput").value.trim();
    if (!path) return;
    try {
      await api("POST", "/api/folders", { path });
      $("folderPathInput").value = "";
      await loadFolders();
      toast("已添加文件夹", "success");
    } catch (err) {
      toast("添加失败：" + err.message, "error");
    }
  });

  $("refreshFoldersBtn").addEventListener("click", () => loadFolders().catch((e) => toast(e.message, "error")));

  async function removeFolder(path) {
    if (!confirm("确定移除 “" + path + "” 吗？仅移除记录，不会删除文件。")) return;
    try {
      await api("DELETE", "/api/folders?path=" + encodeURIComponent(path));
      await loadFolders();
      if ($("filesCard").dataset.folder === path) $("filesCard").hidden = true;
    } catch (err) {
      toast("移除失败：" + err.message, "error");
    }
  }

  // === 文件列表 ===
  async function openFolder(path) {
    const card = $("filesCard");
    card.hidden = false;
    card.dataset.folder = path;
    $("filesFolderName").textContent = "(" + path + ")";
    await loadFiles();
  }
  $("closeFilesBtn").addEventListener("click", () => ($("filesCard").hidden = true));

  async function loadFiles() {
    const card = $("filesCard");
    if (card.hidden) return;
    const path = card.dataset.folder;
    const withHash = $("withHash").checked;
    const tbody = $("filesTbody");
    tbody.innerHTML = '<tr><td colspan="4" class="empty">加载中…</td></tr>';
    try {
      const list = await api(
        "GET",
        "/api/folders/list?path=" + encodeURIComponent(path) + (withHash ? "&hash=1" : ""),
      );
      if (!list.length) {
        tbody.innerHTML = '<tr><td colspan="4" class="empty">空文件夹</td></tr>';
        return;
      }
      tbody.innerHTML = "";
      for (const f of list) {
        const tr = document.createElement("tr");
        tr.innerHTML =
          '<td class="path">' + escapeHTML(f.rel_path) + "</td>" +
          '<td class="num">' + fmtBytes(f.size) + "</td>" +
          "<td>" + fmtTime(f.mtime) + "</td>" +
          '<td class="hash">' + (f.hash ? f.hash.slice(0, 16) + "…" : "—") + "</td>";
        tbody.appendChild(tr);
      }
    } catch (err) {
      tbody.innerHTML = '<tr><td colspan="4" class="empty">加载失败：' + escapeHTML(err.message) + "</td></tr>";
    }
  }
  $("refreshFilesBtn").addEventListener("click", loadFiles);
  $("withHash").addEventListener("change", loadFiles);

  // === 对端 ===
  async function loadPeers() {
    const peers = await api("GET", "/api/peers");
    const ul = $("peerList");
    ul.innerHTML = "";
    if (!peers.length) {
      ul.innerHTML = '<li class="empty">还没有连接任何对端</li>';
      return;
    }
    for (const p of peers) {
      const li = document.createElement("li");
      li.innerHTML =
        '<div class="folder-meta"><div class="folder-path">' + escapeHTML(p.name || "(未命名)") + '</div>' +
        '<div class="folder-stats">ID: ' + escapeHTML(p.id) + ' · ' + escapeHTML(p.address) + '</div></div>';
      const actions = document.createElement("div");
      actions.className = "folder-actions";
      const syncBtn = document.createElement("button");
      syncBtn.className = "btn";
      syncBtn.textContent = "选择同步…";
      syncBtn.onclick = () => chooseFolderForPeer(p);
      const rmBtn = document.createElement("button");
      rmBtn.className = "btn-ghost btn-danger";
      rmBtn.textContent = "断开";
      rmBtn.onclick = async () => {
        try { await api("DELETE", "/api/peers?id=" + encodeURIComponent(p.id)); await loadPeers(); }
        catch (e) { toast(e.message, "error"); }
      };
      actions.appendChild(syncBtn);
      actions.appendChild(rmBtn);
      li.appendChild(actions);
      ul.appendChild(li);
    }
  }
  $("refreshPeersBtn").addEventListener("click", () => {
    loadPeers().catch((e) => toast(e.message, "error"));
    loadDiscovered().catch((e) => toast(e.message, "error"));
  });

  async function loadDiscovered() {
    const list = await api("GET", "/api/peers/discovered");
    const ul = $("discoveredList");
    ul.innerHTML = "";
    if (!list.length) {
      ul.innerHTML = '<li class="empty">未在局域网内发现其他设备（确认对端已启动）</li>';
      return;
    }
    for (const b of list) {
      const li = document.createElement("li");
      const meta = document.createElement("div");
      meta.className = "beacon-meta";
      const id = document.createElement("div");
      id.className = "beacon-id";
      id.textContent = (b.name || "(未命名)") + " — " + b.id;
      const sub = document.createElement("div");
      sub.className = "beacon-sub";
      sub.textContent = (b.address || "(未知地址)") + (b.folder ? " · " + b.folder : "");
      meta.appendChild(id); meta.appendChild(sub);
      li.appendChild(meta);
      const actions = document.createElement("div");
      actions.className = "folder-actions";
      const addBtn = document.createElement("button");
      addBtn.className = "btn-ghost";
      addBtn.textContent = "填入连接";
      addBtn.onclick = () => {
        $("peerIdInput").value = b.id;
        $("peerAddrInput").value = b.address || "";
        $("peerNameInput").value = b.name || "";
        $("peerAddrInput").focus();
      };
      actions.appendChild(addBtn);
      li.appendChild(actions);
      ul.appendChild(li);
    }
  }

  $("connectForm").addEventListener("submit", async (e) => {
    e.preventDefault();
    const id = $("peerIdInput").value.trim();
    const addr = $("peerAddrInput").value.trim();
    const name = $("peerNameInput").value.trim();
    if (!id) { toast("请输入对端 ID", "error"); return; }
    try {
      const r = await api("POST", "/api/peers/connect", { id, address: addr, name });
      toast("已连接：" + (r.name || r.id), "success");
      $("peerIdInput").value = "";
      $("peerAddrInput").value = "";
      $("peerNameInput").value = "";
      await loadPeers();
    } catch (err) {
      toast("连接失败：" + err.message, "error");
    }
  });

  // === 同步 ===
  async function promptSync(folder) {
    const peers = await api("GET", "/api/peers");
    if (!peers.length) { toast("请先连接对端", "error"); return; }
    const choice = prompt(
      "选择要同步的对端（输入 ID）：\n" +
        peers.map((p, i) => "  " + (i + 1) + ". " + (p.name || p.id) + "  [" + p.id + "]").join("\n"),
      peers[0].id,
    );
    if (!choice) return;
    const peer = peers.find((p) => p.id === choice || (p.name || "") === choice);
    if (!peer) { toast("未找到对端：" + choice, "error"); return; }
    startSync(peer.id, folder);
  }

  function chooseFolderForPeer(peer) {
    // 让用户选择本地要推送的文件夹
    api("GET", "/api/folders").then((folders) => {
      if (!folders.length) { toast("请先添加同步文件夹", "error"); return; }
      const choice = prompt(
        "选择要从本端推送到 " + (peer.name || peer.id) + " 的文件夹（输入完整路径）：\n" +
          folders.map((f, i) => "  " + (i + 1) + ". " + f.path).join("\n"),
        folders[0].path,
      );
      if (!choice) return;
      const f = folders.find((x) => x.path === choice);
      if (!f) { toast("未找到文件夹", "error"); return; }
      startSync(peer.id, f.path);
    });
  }

  async function startSync(peerId, folder) {
    if (activeSync) { toast("已有同步任务进行中", "error"); return; }
    // 让用户输入对端文件夹路径（默认与本端相同）
    const remote = prompt(
      "本端要推送的文件夹：\n  " + folder +
        "\n\n请输入对端接收的文件夹路径。\n" +
        "要求：该目录必须已存在于对端机器上（不存在时会自动创建空目录，\n" +
        "      已存在但未注册时本软件会自动注册，无需手动操作）。",
      folder,
    );
    if (remote === null) return;
    // 先建立 SSE 连接，再发送同步请求：
    // 同步 goroutine 在收到 POST 之后会立刻 Publish 事件，
    // 如果先 POST 再 openSSE，会丢失早期事件，导致 UI 一直停在“准备中”。
    openSSE();
    // 拿到对端地址后，先确保对端目录存在并已注册，避免远端返回 403/404。
    // 这样可以：
    //   1. 在同步真正开始前给出明确错误反馈（不卡在“准备中”）
    //   2. 对端缺失目录时自动创建，提升易用性
    const peerInfo = await api("GET", "/api/peers");
    const peer = (peerInfo || []).find((p) => p.id === peerId);
    if (!peer) {
      $("currentFile").textContent = "未找到对端信息";
      toast("未找到对端 " + peerId, "error");
      activeSync = null;
      return;
    }
    try {
      const ensureUrl = peer.address.replace(/\/$/, "") +
        "/api/peer/folder/ensure?folder=" + encodeURIComponent(remote) + "&create=true";
      const r = await fetch(ensureUrl, { method: "POST" });
      if (!r.ok) {
        const t = await r.text();
        // 远端目录创建失败：清晰提示并终止流程
        $("currentFile").textContent = "远端目录准备失败：" + t;
        toast("远端目录准备失败：" + t, "error", 6000);
        activeSync = null;
        return;
      }
      const info = await r.json();
      if (info && info.created) {
        toast("已在对端创建并注册目录：" + info.abs, "success", 4000);
      }
    } catch (err) {
      $("currentFile").textContent = "无法联系对端：" + err.message;
      toast("无法联系对端：" + err.message, "error", 6000);
      activeSync = null;
      return;
    }
    try {
      const r = await api("POST", "/api/sync/start", { peer_id: peerId, folder, remote_folder: remote });
      activeSync = { jobId: r.job_id, peerId, folder, remoteFolder: remote, startedAt: Date.now(), lastEventAt: Date.now() };
      $("syncCard").hidden = false;
      $("syncJobId").textContent = r.job_id;
      $("syncPeer").textContent = peerId;
      $("syncFolder").textContent = folder;
      $("barFill").style.width = "0%";
      $("progressText").textContent = "0 / 0";
      $("sizeText").textContent = "0 B / 0 B";
      $("speedText").textContent = "—";
      $("currentFile").textContent = "准备中…";
      $("fileLog").innerHTML = "";
      ensureSyncWatchdog();
    } catch (err) {
      // 启动失败也要清掉状态，避免 UI 卡住
      $("currentFile").textContent = "启动失败：" + err.message;
      toast("启动同步失败：" + err.message, "error", 6000);
      activeSync = null;
    }
  }

  $("cancelSyncBtn").addEventListener("click", async () => {
    if (!activeSync) return;
    const jobId = activeSync.jobId;
    const btn = $("cancelSyncBtn");
    btn.disabled = true;
    $("currentFile").textContent = "正在取消…";
    // 无论后端响应如何都清理前端状态，避免任务已完成时返回 404 引发卡死
    try {
      await api("POST", "/api/sync/cancel?job_id=" + encodeURIComponent(jobId));
    } catch (e) {
      // 后端取消失败时（如任务已结束），仍视为已取消
      console.warn("cancel request failed:", e);
    } finally {
      activeSync = null;
      btn.disabled = false;
      // 保留 syncCard 几秒后隐藏，方便用户看到“已取消”状态
      setTimeout(() => {
        if (!activeSync) $("syncCard").hidden = true;
      }, 1500);
    }
  });

  function openSSE() {
    if (sseSource) sseSource.close();
    sseSource = new EventSource("/api/events");
    sseSource.addEventListener("info", () => {});
    sseSource.addEventListener("start", (e) => onEvent("start", e));
    sseSource.addEventListener("plan", (e) => onEvent("plan", e));
    sseSource.addEventListener("progress", (e) => onEvent("progress", e));
    sseSource.addEventListener("file_done", (e) => onEvent("file_done", e));
    sseSource.addEventListener("done", (e) => onEvent("done", e));
    // 注意：这里必须用后端的 sync_error 事件名而不是 "error"，
    // 否则会被 EventSource 内置的 error 事件吞掉，错误信息无法到达 UI。
    sseSource.addEventListener("sync_error", (e) => onEvent("sync_error", e));
    sseSource.addEventListener("error", () => {
      // EventSource 内置的 error：连接断开时触发，浏览器会自动重连。
      // 这里只更新状态，不弹错误 toast（避免与真正的业务错误混淆）。
      if (sseSource && sseSource.readyState === EventSource.CLOSED) {
        sseSource = null;
      }
    });
  }

  function onEvent(type, e) {
    let ev;
    try { ev = JSON.parse(e.data); } catch { return; }
    if (activeSync && ev.job_id && ev.job_id !== activeSync.jobId) return;
    if (activeSync) activeSync.lastEventAt = Date.now();
    if (type === "start") {
      $("currentFile").textContent = "正在构建清单…";
    } else if (type === "plan") {
      $("progressText").textContent = "0 / " + ev.total;
      $("sizeText").textContent = "0 B / " + fmtBytes(ev.bytes_total);
    } else if (type === "progress") {
      const pct = ev.total > 0 ? (ev.bytes_done / ev.bytes_total) * 100 : 0;
      $("barFill").style.width = pct.toFixed(1) + "%";
      $("progressText").textContent = ev.index + " / " + ev.total;
      $("sizeText").textContent = fmtBytes(ev.bytes_done) + " / " + fmtBytes(ev.bytes_total);
      $("speedText").textContent = fmtSpeed(ev.speed_bps);
      $("currentFile").textContent = "正在传输：" + ev.file;
    } else if (type === "file_done") {
      const li = document.createElement("li");
      li.className = ev.status === "success" ? "ok" : "fail";
      li.innerHTML =
        '<span class="path">' + escapeHTML(ev.file) + "</span>" +
        (ev.status === "success" ? '<span class="pill ok">OK</span>' : '<span class="pill fail">FAIL</span>') +
        (ev.message ? ' <span class="muted">' + escapeHTML(ev.message) + '</span>' : "");
      $("fileLog").prepend(li);
    } else if (type === "done") {
      $("currentFile").textContent = ev.message || "同步完成";
      $("barFill").style.width = "100%";
      activeSync = null;
      loadJobs();
      toast(ev.message || "同步完成", "success", 5000);
    } else if (type === "sync_error") {
      $("currentFile").textContent = "出错：" + (ev.message || "");
      activeSync = null;
      toast(ev.message || "同步出错", "error", 5000);
    }
  }

  // === 同步超时监控 ===
  // SSE 在某些边界场景下可能丢事件（连接被重置、buffer 满丢弃、页面切换等），
  // 表现为 UI 卡在“准备中”。此 watchdog 定期检查任务是否已在后端结束，
  // 若是则主动恢复 UI；若长时间无事件则提示用户。
  let syncWatchdogTimer = null;
  const SYNC_STALL_MS = 30000; // 30s 无任何事件视为“疑似卡住”
  const SYNC_HARD_TIMEOUT_MS = 120000; // 120s 无任何事件强制清理
  function ensureSyncWatchdog() {
    if (syncWatchdogTimer) clearInterval(syncWatchdogTimer);
    syncWatchdogTimer = setInterval(checkSyncProgress, 3000);
  }
  function stopSyncWatchdog() {
    if (syncWatchdogTimer) {
      clearInterval(syncWatchdogTimer);
      syncWatchdogTimer = null;
    }
  }
  async function checkSyncProgress() {
    if (!activeSync) {
      stopSyncWatchdog();
      return;
    }
    const now = Date.now();
    const sinceLast = now - (activeSync.lastEventAt || activeSync.startedAt || now);
    if (sinceLast < SYNC_STALL_MS) return; // 还在正常节奏内，不打扰
    // 拉取一次后端任务列表，确认任务状态
    let jobs = [];
    try {
      jobs = await api("GET", "/api/sync/jobs");
    } catch (_) {
      return;
    }
    const me = activeSync.jobId;
    const job = (jobs || []).find((j) => j.job_id === me);
    if (job && job.status === "done") {
      // 后端已完成，但 SSE 没收到 done/sync_error 事件，主动恢复 UI
      $("currentFile").textContent = "同步完成（事件已恢复）";
      $("barFill").style.width = "100%";
      activeSync = null;
      stopSyncWatchdog();
      loadJobs();
      toast("同步已完成", "success", 5000);
      return;
    }
    if (sinceLast >= SYNC_HARD_TIMEOUT_MS) {
      // 长时间无事件，强制清理并尝试取消后端任务
      const jobId = activeSync.jobId;
      activeSync = null;
      stopSyncWatchdog();
      $("currentFile").textContent = "同步超时未响应，已自动取消";
      try {
        await api("POST", "/api/sync/cancel?job_id=" + encodeURIComponent(jobId));
      } catch (_) {}
      toast("同步超时，已自动取消", "error", 5000);
      return;
    }
    // 任务仍在进行（building manifest / transfer），提示但不打断
    $("currentFile").textContent = "等待后端响应…（已 " + Math.floor(sinceLast / 1000) + "s 未收到事件）";
  }

  // === 任务日志 ===
  async function loadJobs() {
    try {
      const jobs = await api("GET", "/api/sync/jobs");
      const ul = $("jobList");
      ul.innerHTML = "";
      if (!jobs.length) {
        ul.innerHTML = '<li class="empty">尚未完成任何同步任务</li>';
        return;
      }
      for (const j of jobs.reverse()) {
        const li = document.createElement("li");
        const left = document.createElement("div");
        left.innerHTML =
          '<div class="job-id">' + escapeHTML(j.job_id) + '</div>' +
          '<div class="job-meta">' + (j.size ? fmtBytes(j.size) : "") + '</div>';
        const actions = document.createElement("div");
        actions.className = "folder-actions";
        const viewBtn = document.createElement("button");
        viewBtn.className = "btn-ghost";
        viewBtn.textContent = "查看";
        viewBtn.onclick = () => window.open("/api/sync/log?job_id=" + encodeURIComponent(j.job_id));
        const dlBtn = document.createElement("button");
        dlBtn.className = "btn";
        dlBtn.textContent = "下载";
        dlBtn.onclick = () => window.open("/api/sync/log/download?job_id=" + encodeURIComponent(j.job_id));
        actions.appendChild(viewBtn);
        actions.appendChild(dlBtn);
        li.appendChild(left);
        li.appendChild(actions);
        ul.appendChild(li);
      }
    } catch (e) {
      /* ignore */
    }
  }
  $("refreshJobsBtn").addEventListener("click", loadJobs);

  function escapeHTML(s) {
    return String(s)
      .replace(/&/g, "&amp;")
      .replace(/</g, "&lt;")
      .replace(/>/g, "&gt;")
      .replace(/"/g, "&quot;");
  }

  // === 启动 ===
  (async () => {
    try {
      await loadInfo();
      await Promise.all([loadFolders(), loadPeers(), loadDiscovered(), loadJobs()]);
    } catch (err) {
      toast("初始化失败：" + err.message, "error");
    }
    // 定期刷新发现
    setInterval(() => loadDiscovered().catch(() => {}), 4000);
  })();
})();
