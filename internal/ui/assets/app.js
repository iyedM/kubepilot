(function () {
  "use strict";

  const listEl = document.getElementById("incident-list");
  const countEl = document.getElementById("incident-count");
  const detailEl = document.getElementById("detail-pane");
  const filterStatus = document.getElementById("filter-status");
  const filterCause = document.getElementById("filter-cause");
  const filterSearch = document.getElementById("filter-search");
  const form = document.getElementById("diagnose-form");
  const errEl = document.getElementById("diagnose-error");
  const btnRefresh = document.getElementById("btn-refresh");
  const btnTheme = document.getElementById("btn-theme");
  const btnToggleDrawer = document.getElementById("btn-toggle-drawer");
  const drawerEl = document.getElementById("diagnose-drawer");

  const btnSeedDev = document.getElementById("btn-seed-dev");
  const btnSeedTest = document.getElementById("btn-seed-test");
  const btnSeedAll = document.getElementById("btn-seed-all");

  const distBar = document.getElementById("dist-bar");
  const distLegend = document.getElementById("dist-legend");
  const btnClearCause = document.getElementById("btn-clear-cause");

  const kpiTotal = document.getElementById("kpi-total");
  const kpiSubTotal = document.getElementById("kpi-sub-total");
  const kpiDiagnosed = document.getElementById("kpi-diagnosed");
  const kpiSubDiagnosed = document.getElementById("kpi-sub-diagnosed");
  const kpiHealthy = document.getElementById("kpi-healthy");
  const kpiSignals = document.getElementById("kpi-signals");
  const kpiSubSignals = document.getElementById("kpi-sub-signals");
  const kpiWaste = document.getElementById("kpi-waste");

  let activeID = null;
  let activeTab = "overview";
  let allIncidents = [];

  const CAUSE_META = {
    cpu_throttling: {
      title: "CPU Throttling (CFS Quota Saturation)",
      short: "CPU Throttling",
      color: "#f43f5e",
      stage: "model-server",
      layer: "Container Cgroup CPU Limit",
      explain:
        "The container's Linux CFS CPU quota limit is too low for inference demand, stalling threads across a high fraction of 100ms periods and inflating token generation latency.",
      verify:
        "Inspect resources.limits.cpu on deployment/model-server and compare against container_cpu_cfs_throttled_periods_total in Prometheus.",
    },
    oom_killed: {
      title: "Container Out-of-Memory (OOMKilled · Exit 137)",
      short: "OOMKilled (137)",
      color: "#e11d48",
      stage: "workload",
      layer: "Container Memory Limit / KV Cache",
      explain:
        "The Linux kernel OOM killer terminated the container (exit code 137) because model weights, KV cache, or application memory exceeded resources.limits.memory.",
      verify:
        "Check pod lastState.terminated.reason == OOMKilled and increase resources.limits.memory or reduce model context/batch size.",
    },
    unschedulable: {
      title: "Pod Unschedulable (Pending Scheduling)",
      short: "Unschedulable",
      color: "#f59e0b",
      stage: "node",
      layer: "Kubernetes Scheduler / Node Capacity / Taints / PVC",
      explain:
        "Kubernetes could not place the pod onto any worker node due to insufficient CPU/memory, unmatched nodeSelector/affinity, untolerated node taints, or an unbound PersistentVolumeClaim.",
      verify:
        "Inspect Warning FailedScheduling events on the Pending pod and check node taints, selectors, or PVC binding status.",
    },
    readiness_failing: {
      title: "Failing or Delayed Readiness Probe",
      short: "Readiness Fail",
      color: "#eab308",
      stage: "model-server",
      layer: "Kubernetes Kubelet Probe / Service Endpoint",
      explain:
        "The pod is Running, but its readinessProbe is failing (HTTP 4xx/5xx, connection refused on wrong port) or delayed by an excessive initialDelaySeconds, keeping it out of Service endpoints.",
      verify:
        "Check Warning Unhealthy events and verify readinessProbe.httpGet.path, port, and initialDelaySeconds in the Pod spec.",
    },
    cold_start: {
      title: "Slow Model Cold Start (Init / Image Pull Delay)",
      short: "Cold Start",
      color: "#a855f7",
      stage: "model-server",
      layer: "Pod Initialization / Model Weight Fetch",
      explain:
        "Traffic failed or stalled during a rollout because an initContainer (model weight download) or uncached container image pull delayed main container startup before the pod became Ready.",
      verify:
        "Compare initContainerStatuses[].state.terminated.{startedAt,finishedAt} and pre-cache model weights on a persistent volume or node image cache.",
    },
    config_change: {
      title: "Serving Configuration Regression (Rollout)",
      short: "Config Change",
      color: "#38bdf8",
      stage: "model-server",
      layer: "Model Server Runtime Flags",
      explain:
        "A new Deployment rollout changed model-server runtime arguments (e.g. --threads 1), reducing token generation speed (llamacpp:predicted_tokens_seconds) without saturating CPU or memory.",
      verify:
        "Diff container args/env between the previous and current ReplicaSet pods and inspect llamacpp:predicted_tokens_seconds.",
    },
    host_cpu_contention: {
      title: "Host CPU Contention (Noisy Neighbor on Node)",
      short: "Host Contention",
      color: "#fb923c",
      stage: "node",
      layer: "Worker Node Kernel Scheduler / Linux PSI",
      explain:
        "The pod is not hitting its own CFS CPU limit, but threads are waiting on the host CPU runqueue (high container_pressure_cpu_waiting_seconds_total) due to another workload saturating the same worker node.",
      verify:
        "Check Linux PSI CPU waiting metrics and top CPU-consuming pods on the same worker node.",
    },
    unknown_no_infrastructure_cause: {
      title: "Application Pre-Inference Delay (Code / Hook Inside App)",
      short: "App Code Delay",
      color: "#6366f1",
      stage: "chat-api",
      layer: "Application Code (Before LLM Client Span)",
      explain:
        "All Kubernetes pods, node resources, and model-server inference latencies are healthy, but OpenTelemetry traces show a large time gap between the incoming HTTP server span start and the outgoing GenAI client span start.",
      verify:
        "Inspect application middleware, synchronous sleeps, or pre-processing logic in chat-api before the OpenAI client call.",
    },
  };

  function getCauseMeta(cause) {
    return (
      CAUSE_META[cause] || {
        title: cause || "Healthy Window (No Fault Detected)",
        short: "Healthy (0 FP)",
        color: "#10b981",
        stage: "none",
        layer: "Abstention Guard (No Fault Detected)",
        explain:
          "No deterministic rule matched the telemetry snapshot. Kubernetes pods, events, Prometheus metrics, and GenAI traces in the target namespace show no infrastructure or application fault (any unrelated noise in other namespaces was ignored).",
        verify: "No action required; workload is operating normally.",
      }
    );
  }

  function escapeHTML(str) {
    return String(str == null ? "" : str)
      .replace(/&/g, "&amp;")
      .replace(/</g, "&lt;")
      .replace(/>/g, "&gt;")
      .replace(/"/g, "&quot;");
  }

  function formatDurationSec(sec) {
    const s = Math.round(Number(sec) || 0);
    if (s < 60) return s + "s";
    const m = Math.floor(s / 60);
    const rem = s % 60;
    return rem > 0 ? m + "m " + rem + "s" : m + "m";
  }

  function formatShortTime(iso) {
    if (!iso || iso === "-") return "-";
    const d = new Date(iso);
    if (isNaN(d.getTime())) return iso;
    return d.toISOString().replace("T", " ").replace(/\.\d+Z$/, " UTC");
  }

  function formatHMS(iso) {
    if (!iso) return "";
    const d = new Date(iso);
    if (isNaN(d.getTime())) return "";
    return d.toISOString().slice(11, 19);
  }

  // Theme toggle
  const savedTheme = localStorage.getItem("kp-theme") || "dark";
  document.documentElement.setAttribute("data-theme", savedTheme);
  btnTheme.textContent = savedTheme === "dark" ? "Light Mode" : "Dark Mode";
  btnTheme.addEventListener("click", function () {
    const current = document.documentElement.getAttribute("data-theme") || "dark";
    const next = current === "dark" ? "light" : "dark";
    document.documentElement.setAttribute("data-theme", next);
    localStorage.setItem("kp-theme", next);
    btnTheme.textContent = next === "dark" ? "Light Mode" : "Dark Mode";
  });

  if (btnToggleDrawer && drawerEl) {
    btnToggleDrawer.addEventListener("click", function () {
      drawerEl.classList.toggle("hidden");
    });
  }

  async function seedDataset(dir, label) {
    try {
      const resp = await fetch("/api/v1/seed", {
        method: "POST",
        headers: { "Content-Type": "application/json" },
        body: JSON.stringify({ dir: dir, limit: 100, clear: true }),
      });
      if (!resp.ok) throw new Error("HTTP " + resp.status);
      activeID = null;
      kpiSubTotal.textContent = label;
      await fetchIncidents();
    } catch (e) {
      errEl.textContent = "Failed to seed dataset: " + e.message;
      errEl.classList.remove("hidden");
    }
  }

  if (btnSeedDev) {
    btnSeedDev.addEventListener("click", function () {
      seedDataset("eval/runs/dev", "Dev split (19 runs)");
    });
  }
  if (btnSeedTest) {
    btnSeedTest.addEventListener("click", function () {
      seedDataset("eval/runs/test", "Held-out Test split (45 runs)");
    });
  }
  if (btnSeedAll) {
    btnSeedAll.addEventListener("click", function () {
      seedDataset("all", "All 64 evaluation runs (Dev + Test)");
    });
  }

  async function fetchIncidents() {
    const params = new URLSearchParams();
    if (filterStatus.value) params.set("status", filterStatus.value);
    if (filterCause.value) params.set("cause", filterCause.value);
    const url = "/api/v1/incidents" + (params.toString() ? "?" + params.toString() : "");
    const resp = await fetch(url);
    if (!resp.ok) {
      throw new Error("HTTP " + resp.status);
    }
    const data = await resp.json();
    allIncidents = data.incidents || [];
    updateKPIsAndDistribution(allIncidents);
    applySearchAndRender();
  }

  function updateKPIsAndDistribution(incidents) {
    let diag = 0;
    let healthy = 0;
    let waste = 0;
    let confSum = 0;
    let totalTraces = 0;
    let totalEvents = 0;
    let totalPods = 0;
    const causeCounts = {};

    incidents.forEach(function (inc) {
      const st = inc.report && inc.report.status;
      const top =
        inc.report && inc.report.topCauses && inc.report.topCauses.length > 0
          ? inc.report.topCauses[0]
          : null;
      if (st === "diagnosed" && top) {
        diag++;
        confSum += Number(top.confidence) || 0;
        causeCounts[top.cause] = (causeCounts[top.cause] || 0) + 1;
      } else {
        healthy++;
        causeCounts["__healthy__"] = (causeCounts["__healthy__"] || 0) + 1;
      }
      if (inc.cost && inc.cost.estimatedWasteCost) {
        waste += Number(inc.cost.estimatedWasteCost) || 0;
      }
      totalTraces += Number(inc.traceCount) || 0;
      totalEvents += Number(inc.eventCount) || 0;
      totalPods += Number(inc.podCount) || 0;
    });

    kpiTotal.textContent = String(incidents.length);
    kpiDiagnosed.textContent = String(diag);
    if (kpiSubDiagnosed) {
      const avgConf = diag > 0 ? Math.round((confSum / diag) * 100) : 0;
      kpiSubDiagnosed.textContent =
        diag > 0 ? "Avg confidence " + avgConf + "% across faults" : "No active fault";
    }
    kpiHealthy.textContent = String(healthy);
    if (kpiSignals) {
      kpiSignals.textContent = String(totalTraces + totalEvents);
    }
    if (kpiSubSignals) {
      kpiSubSignals.textContent =
        totalTraces + " traces · " + totalEvents + " K8s events · " + totalPods + " pods";
    }
    kpiWaste.textContent = "$" + waste.toFixed(4);

    renderDistributionBar(incidents.length, causeCounts);
  }

  function renderDistributionBar(total, causeCounts) {
    if (!distBar || !distLegend) return;
    if (total === 0) {
      distBar.innerHTML = "";
      distLegend.innerHTML = "";
      return;
    }
    const order = [
      "cpu_throttling",
      "oom_killed",
      "unschedulable",
      "readiness_failing",
      "cold_start",
      "config_change",
      "host_cpu_contention",
      "unknown_no_infrastructure_cause",
      "__healthy__",
    ];

    const segs = [];
    const pills = [];
    order.forEach(function (key) {
      const cnt = causeCounts[key] || 0;
      if (cnt === 0) return;
      const pct = ((cnt / total) * 100).toFixed(1);
      const isHealthyKey = key === "__healthy__";
      const meta = isHealthyKey ? getCauseMeta("") : getCauseMeta(key);
      const isActive =
        (!isHealthyKey && filterCause.value === key) ||
        (isHealthyKey && filterStatus.value === "unknown");

      segs.push(
        '<div class="dist-seg" data-cause="' +
          escapeHTML(key) +
          '" style="width:' +
          pct +
          "%;background:" +
          meta.color +
          '" title="' +
          escapeHTML(meta.short + ": " + cnt + " (" + pct + "%)") +
          '"></div>'
      );

      pills.push(
        '<button type="button" class="dist-pill' +
          (isActive ? " active" : "") +
          '" data-cause="' +
          escapeHTML(key) +
          '">' +
          '<span class="dist-swatch" style="background:' +
          meta.color +
          '"></span>' +
          "<span>" +
          escapeHTML(meta.short) +
          "</span>" +
          "<strong>" +
          cnt +
          "</strong>" +
          "</button>"
      );
    });

    distBar.innerHTML = segs.join("");
    distLegend.innerHTML = pills.join("");

    if (btnClearCause) {
      btnClearCause.classList.toggle("hidden", !filterCause.value && !filterStatus.value);
    }

    function handleFilterClick(key) {
      if (key === "__healthy__") {
        filterCause.value = "";
        filterStatus.value = filterStatus.value === "unknown" ? "" : "unknown";
      } else {
        filterStatus.value = "";
        filterCause.value = filterCause.value === key ? "" : key;
      }
      activeID = null;
      fetchIncidents();
    }

    Array.from(distBar.querySelectorAll(".dist-seg")).forEach(function (el) {
      el.addEventListener("click", function () {
        handleFilterClick(el.getAttribute("data-cause"));
      });
    });
    Array.from(distLegend.querySelectorAll(".dist-pill")).forEach(function (el) {
      el.addEventListener("click", function () {
        handleFilterClick(el.getAttribute("data-cause"));
      });
    });
  }

  if (btnClearCause) {
    btnClearCause.addEventListener("click", function () {
      filterCause.value = "";
      filterStatus.value = "";
      activeID = null;
      fetchIncidents();
    });
  }

  function applySearchAndRender() {
    const q = (filterSearch.value || "").trim().toLowerCase();
    const filtered = allIncidents.filter(function (inc) {
      if (!q) return true;
      const top =
        inc.report && inc.report.topCauses && inc.report.topCauses.length > 0
          ? inc.report.topCauses[0]
          : null;
      const hay = [
        inc.id,
        inc.runId,
        inc.namespace,
        top ? top.cause : "healthy unknown",
        top ? getCauseMeta(top.cause).title : "",
        top && top.component ? top.component.workload : "",
        top && top.component ? top.component.pod : "",
      ]
        .join(" ")
        .toLowerCase();
      return hay.indexOf(q) !== -1;
    });

    countEl.textContent = String(filtered.length);
    renderIncidentList(filtered);
    if (filtered.length > 0 && !activeID) {
      selectIncident(filtered[0].id);
    }
  }

  function renderIncidentList(incidents) {
    if (incidents.length === 0) {
      listEl.innerHTML = '<li class="empty-state">No incidents match current filters.</li>';
      return;
    }
    listEl.innerHTML = incidents
      .map(function (inc) {
        const status = (inc.report && inc.report.status) || "unknown";
        const top =
          inc.report && inc.report.topCauses && inc.report.topCauses.length > 0
            ? inc.report.topCauses[0]
            : null;
        const meta = top ? getCauseMeta(top.cause) : getCauseMeta("");
        const humanTitle = top ? meta.title : "Healthy Window (No Fault Blamed)";
        const confBadge = top
          ? Math.round(top.confidence * 100) + "% conf"
          : "Healthy · 0 FP";
        const activeClass = inc.id === activeID ? " active" : "";
        const borderClass = status === "diagnosed" ? " is-diagnosed" : " is-healthy";
        const statusClass =
          status === "diagnosed" ? "status-diagnosed" : "status-unknown";
        const workload =
          top && top.component && top.component.workload
            ? top.component.workload
            : "ns/" + (inc.namespace || "genai");

        return (
          '<li class="incident-item' +
          activeClass +
          borderClass +
          '" data-id="' +
          escapeHTML(inc.id) +
          '">' +
          '<div class="incident-item-top">' +
          '<span class="incident-human-title">' +
          escapeHTML(humanTitle) +
          "</span>" +
          '<span class="status-tag ' +
          statusClass +
          '">' +
          escapeHTML(confBadge) +
          "</span>" +
          "</div>" +
          '<div class="incident-id">' +
          escapeHTML(inc.id) +
          "</div>" +
          '<div class="meta-row">' +
          "<span>" +
          escapeHTML(workload) +
          "</span>" +
          "<span>" +
          escapeHTML(inc.traceCount || 0) +
          " traces · " +
          escapeHTML(inc.eventCount || 0) +
          " events</span>" +
          "</div>" +
          "</li>"
        );
      })
      .join("");

    Array.from(listEl.querySelectorAll(".incident-item")).forEach(function (el) {
      el.addEventListener("click", function () {
        selectIncident(el.getAttribute("data-id"));
      });
    });
  }

  async function selectIncident(id) {
    activeID = id;
    Array.from(listEl.querySelectorAll(".incident-item")).forEach(function (el) {
      el.classList.toggle("active", el.getAttribute("data-id") === id);
    });
    const resp = await fetch("/api/v1/incidents/" + encodeURIComponent(id));
    if (!resp.ok) {
      detailEl.innerHTML =
        '<div class="alert-error">Failed to load incident ' +
        escapeHTML(id) +
        "</div>";
      return;
    }
    const inc = await resp.json();
    renderIncidentDetail(inc);
  }

  function renderRadialGaugeSVG(pct, isDiagnosed) {
    const r = 24;
    const c = 2 * Math.PI * r;
    const fillPct = isDiagnosed ? pct : 100;
    const offset = c - (fillPct / 100) * c;
    const strokeColor = isDiagnosed ? "#f43f5e" : "#10b981";
    const label = isDiagnosed ? pct + "%" : "0 FP";
    return (
      '<div class="gauge-box">' +
      '<svg width="64" height="64" viewBox="0 0 64 64">' +
      '<circle cx="32" cy="32" r="' +
      r +
      '" fill="none" stroke="rgba(148,163,184,0.2)" stroke-width="6"/>' +
      '<circle cx="32" cy="32" r="' +
      r +
      '" fill="none" stroke="' +
      strokeColor +
      '" stroke-width="6" stroke-linecap="round" stroke-dasharray="' +
      c.toFixed(1) +
      '" stroke-dashoffset="' +
      offset.toFixed(1) +
      '" transform="rotate(-90 32 32)"/>' +
      '<text x="32" y="36" text-anchor="middle" fill="currentColor" font-size="12" font-weight="800" font-family="monospace">' +
      escapeHTML(label) +
      "</text>" +
      "</svg>" +
      '<span class="gauge-label">' +
      (isDiagnosed ? "Confidence" : "Healthy") +
      "</span>" +
      "</div>"
    );
  }

  function buildTopologyHTML(inc, topCause, topMeta) {
    const tel = inc.telemetry || {};
    const reqs = tel.requests || [];
    const traces = tel.traces || [];
    const pods = tel.pods || [];

    // Compute Request / Loadgen stats
    let errReqs = 0;
    let latencies = [];
    reqs.forEach(function (r) {
      if (r.status >= 400 || r.status === 0 || r.error) errReqs++;
      if (r.clientLatencyMs > 0) latencies.push(r.clientLatencyMs);
    });
    latencies.sort(function (a, b) {
      return a - b;
    });
    const p95ReqMs =
      latencies.length > 0
        ? Math.round(latencies[Math.floor(latencies.length * 0.95)])
        : 0;
    const errRatePct = reqs.length > 0 ? Math.round((errReqs / reqs.length) * 100) : 0;

    // Compute chat-api & model-server pod stats
    let chatPod = null;
    let modelPod = null;
    pods.forEach(function (p) {
      if (p.namespace !== (inc.namespace || "genai")) return;
      if ((p.name || "").indexOf("chat-api") !== -1) chatPod = p;
      if ((p.name || "").indexOf("model-server") !== -1) modelPod = p;
    });

    let maxGapMs = 0;
    let maxInfMs = 0;
    traces.forEach(function (t) {
      if (t.preInferenceGapMs > maxGapMs) maxGapMs = Math.round(t.preInferenceGapMs);
      if (t.inferenceMs > maxInfMs) maxInfMs = Math.round(t.inferenceMs);
    });

    // Determine which node in the 4-stage pipeline is faulted
    let faultStage = "none";
    if (topCause) {
      const compWorkload = (topCause.component && topCause.component.workload) || "";
      if (topCause.cause === "unknown_no_infrastructure_cause") {
        faultStage = "chat-api";
      } else if (
        topCause.cause === "unschedulable" ||
        topCause.cause === "host_cpu_contention"
      ) {
        faultStage = "node";
      } else if (compWorkload.indexOf("chat-api") !== -1) {
        faultStage = "chat-api";
      } else {
        faultStage = "model-server";
      }
    }

    function stageClass(stage) {
      if (faultStage === stage) return "topo-node node-fault";
      if (faultStage !== "none" && stage === "client" && errRatePct > 0) {
        return "topo-node node-warn";
      }
      return "topo-node node-ok";
    }

    function stageBadge(stage, labelOk) {
      if (faultStage === stage && topCause) {
        return (
          '<div class="topo-fault-badge">FAULT: ' +
          escapeHTML(topMeta.short.toUpperCase()) +
          "</div>"
        );
      }
      return '<div class="topo-ok-badge">' + escapeHTML(labelOk) + "</div>";
    }

    const chatCont =
      chatPod && chatPod.containers && chatPod.containers[0]
        ? chatPod.containers[0]
        : null;
    const modelCont =
      modelPod && modelPod.containers && modelPod.containers[0]
        ? modelPod.containers[0]
        : null;

    const chatMemLim =
      chatCont && chatCont.limitsMemoryBytes
        ? Math.round(chatCont.limitsMemoryBytes / (1024 * 1024)) + " MiB"
        : "256 MiB";
    const modelCpuLim =
      modelCont && modelCont.limitsCpuMillis
        ? modelCont.limitsCpuMillis + "m"
        : "2000m";
    const modelMemLim =
      modelCont && modelCont.limitsMemoryBytes
        ? Math.round(modelCont.limitsMemoryBytes / (1024 * 1024)) + " MiB"
        : "1536 MiB";

    return (
      '<div class="section-card">' +
      '<div class="section-card-header">' +
      '<h3 class="section-card-title">Interactive GenAI Service &amp; Infrastructure Blast-Radius Map</h3>' +
      '<span class="pill">Live Telemetry Correlation</span>' +
      "</div>" +
      '<div class="topology-grid">' +
      // Stage 1: LoadGen Client
      '<div class="' +
      stageClass("client") +
      '">' +
      '<div class="topo-node-header">' +
      '<div><div class="topo-node-kind">1. Client Traffic</div><div class="topo-node-name">loadgen</div></div>' +
      '<span class="pill">' +
      reqs.length +
      " reqs</span>" +
      "</div>" +
      '<div class="topo-metrics">' +
      '<div class="topo-metric-row"><span>HTTP Error Rate:</span><strong>' +
      errRatePct +
      "%</strong></div>" +
      '<div class="topo-metric-row"><span>P95 Client Latency:</span><strong>' +
      p95ReqMs +
      " ms</strong></div>" +
      '<div class="topo-metric-row"><span>Exported Traces:</span><strong>' +
      traces.length +
      "</strong></div>" +
      "</div>" +
      (errRatePct > 0
        ? '<div class="topo-fault-badge" style="background:var(--amber)">SYMPTOM: ' +
          errRatePct +
          "% ERRORS / SLOW</div>"
        : '<div class="topo-ok-badge">200 OK STREAM</div>') +
      "</div>" +
      // Arrow 1->2
      '<div class="topo-arrow"><span>HTTP POST</span><strong>/chat</strong><span>&rarr;</span></div>' +
      // Stage 2: chat-api
      '<div class="' +
      stageClass("chat-api") +
      '">' +
      '<div class="topo-node-header">' +
      '<div><div class="topo-node-kind">2. Gateway Pod</div><div class="topo-node-name">deployment/chat-api</div></div>' +
      '<span class="pill">' +
      escapeHTML(chatPod ? chatPod.phase : "Running") +
      "</span>" +
      "</div>" +
      '<div class="topo-metrics">' +
      '<div class="topo-metric-row"><span>Max Pre-Inference Gap:</span><strong>' +
      maxGapMs +
      " ms</strong></div>" +
      '<div class="topo-metric-row"><span>Memory Limit:</span><strong>' +
      escapeHTML(chatMemLim) +
      "</strong></div>" +
      '<div class="topo-metric-row"><span>Restarts:</span><strong>' +
      (chatCont ? chatCont.restartCount : 0) +
      "</strong></div>" +
      "</div>" +
      stageBadge("chat-api", "OTEL SPAN HEALTHY") +
      "</div>" +
      // Arrow 2->3
      '<div class="topo-arrow"><span>GenAI OTel</span><strong>/v1/chat</strong><span>&rarr;</span></div>' +
      // Stage 3: model-server
      '<div class="' +
      stageClass("model-server") +
      '">' +
      '<div class="topo-node-header">' +
      '<div><div class="topo-node-kind">3. LLM Inference Pod</div><div class="topo-node-name">deployment/model-server</div></div>' +
      '<span class="pill">' +
      escapeHTML(modelPod ? modelPod.phase : "Running") +
      "</span>" +
      "</div>" +
      '<div class="topo-metrics">' +
      '<div class="topo-metric-row"><span>Max LLM Inference:</span><strong>' +
      maxInfMs +
      " ms</strong></div>" +
      '<div class="topo-metric-row"><span>CPU / RAM Limit:</span><strong>' +
      escapeHTML(modelCpuLim + " / " + modelMemLim) +
      "</strong></div>" +
      '<div class="topo-metric-row"><span>Container Ready:</span><strong>' +
      (modelCont ? (modelCont.ready ? "true" : "false") : "true") +
      "</strong></div>" +
      "</div>" +
      stageBadge("model-server", "INFERENCE HEALTHY") +
      "</div>" +
      // Arrow 3->4
      '<div class="topo-arrow"><span>Scheduled</span><strong>Kubelet</strong><span>&rarr;</span></div>' +
      // Stage 4: Worker Node / Scheduler
      '<div class="' +
      stageClass("node") +
      '">' +
      '<div class="topo-node-header">' +
      '<div><div class="topo-node-kind">4. K8s Node &amp; Kernel</div><div class="topo-node-name">' +
      escapeHTML((modelPod && modelPod.nodeName) || "kind-worker") +
      "</div></div>" +
      '<span class="pill">Linux PSI</span>' +
      "</div>" +
      '<div class="topo-metrics">' +
      '<div class="topo-metric-row"><span>Namespace:</span><strong>' +
      escapeHTML(inc.namespace || "genai") +
      "</strong></div>" +
      '<div class="topo-metric-row"><span>K8s Events in Window:</span><strong>' +
      escapeHTML(inc.eventCount || 0) +
      "</strong></div>" +
      '<div class="topo-metric-row"><span>Pods Evaluated:</span><strong>' +
      escapeHTML(inc.podCount || 0) +
      "</strong></div>" +
      "</div>" +
      stageBadge("node", "NODE & SCHEDULER OK") +
      "</div>" +
      "</div>" +
      "</div>"
    );
  }

  function buildSwimlaneHTML(inc) {
    const rep = inc.report || {};
    const win = rep.window || {};
    const tel = inc.telemetry || {};
    const winStart = win.from ? new Date(win.from).getTime() : 0;
    const winEnd = win.to ? new Date(win.to).getTime() : 0;
    const spanMs = Math.max(1000, winEnd - winStart);

    function pctPos(iso) {
      if (!iso || !winStart) return 50;
      const t = new Date(iso).getTime();
      if (isNaN(t)) return 50;
      const p = ((t - winStart) / spanMs) * 100;
      return Math.max(2, Math.min(98, p));
    }

    // Lane 1: Client HTTP Requests
    const reqPins = (tel.requests || [])
      .map(function (r) {
        const isErr = r.status >= 400 || r.status === 0 || r.error;
        const col = isErr ? "#f43f5e" : "#10b981";
        return (
          '<div class="swimlane-pin" style="left:' +
          pctPos(r.ts).toFixed(1) +
          "%;background:" +
          col +
          '" title="Request ' +
          escapeHTML(r.promptId || "") +
          " @ " +
          escapeHTML(formatHMS(r.ts)) +
          " · HTTP " +
          escapeHTML(r.status) +
          " (" +
          Math.round(r.clientLatencyMs || 0) +
          ' ms)"></div>'
        );
      })
      .join("");

    // Lane 2: OTel GenAI Traces
    const tracePins = (tel.traces || [])
      .map(function (t) {
        const isSlow =
          t.error || (t.inferenceMs || 0) > 15000 || (t.preInferenceGapMs || 0) > 1500;
        const col = isSlow ? "#f59e0b" : "#38bdf8";
        return (
          '<div class="swimlane-pin" style="left:' +
          pctPos(t.startTime).toFixed(1) +
          "%;background:" +
          col +
          '" title="Trace ' +
          escapeHTML((t.traceId || "").slice(0, 12)) +
          " · Inference " +
          Math.round(t.inferenceMs || 0) +
          "ms · Gap " +
          Math.round(t.preInferenceGapMs || 0) +
          'ms"></div>'
        );
      })
      .join("");

    // Lane 3: Rule Correlated Evidence Pins
    const evPins = [];
    (rep.topCauses || []).forEach(function (c) {
      (c.evidence || []).forEach(function (ev) {
        if (!ev.timestamp) return;
        evPins.push(
          '<div class="swimlane-pin" style="left:' +
            pctPos(ev.timestamp).toFixed(1) +
            '%;background:#a855f7;width:10px" title="[' +
            escapeHTML(ev.source) +
            " @ " +
            escapeHTML(formatHMS(ev.timestamp)) +
            "] " +
            escapeHTML(ev.summary) +
            '"></div>'
        );
      });
    });

    // Lane 4: Kubernetes Events
    const k8sPins = (tel.events || [])
      .map(function (e) {
        const col = e.type === "Warning" ? "#f43f5e" : "#38bdf8";
        return (
          '<div class="swimlane-pin" style="left:' +
          pctPos(e.timestamp).toFixed(1) +
          "%;background:" +
          col +
          '" title="' +
          escapeHTML(e.type + " " + e.reason + " (" + e.involvedName + "): " + e.message) +
          '"></div>'
        );
      })
      .join("");

    const durSec = Math.round(spanMs / 1000);
    return (
      '<div class="section-card">' +
      '<div class="section-card-header">' +
      '<h3 class="section-card-title">Synchronized Multi-Signal Timeline Swimlanes</h3>' +
      '<span class="pill">' +
      escapeHTML(formatHMS(win.from)) +
      " &rarr; " +
      escapeHTML(formatHMS(win.to)) +
      " (" +
      durSec +
      "s window)</span>" +
      "</div>" +
      '<div class="swimlane-wrap">' +
      '<div class="swimlane-row"><div class="swimlane-label"><span class="dot dot-green"></span> 1. HTTP Requests</div><div class="swimlane-track">' +
      reqPins +
      "</div></div>" +
      '<div class="swimlane-row"><div class="swimlane-label"><span class="dot dot-blue"></span> 2. OTel GenAI Spans</div><div class="swimlane-track">' +
      tracePins +
      "</div></div>" +
      '<div class="swimlane-row"><div class="swimlane-label"><span class="dot" style="background:var(--purple)"></span> 3. Rule Proof Pins</div><div class="swimlane-track">' +
      evPins.join("") +
      "</div></div>" +
      '<div class="swimlane-row"><div class="swimlane-label"><span class="dot dot-amber"></span> 4. K8s Events</div><div class="swimlane-track">' +
      k8sPins +
      "</div></div>" +
      "</div>" +
      '<div class="swimlane-axis">' +
      "<span>T+0s (" +
      escapeHTML(formatHMS(win.from)) +
      ")</span>" +
      "<span>T+" +
      Math.round(durSec * 0.25) +
      "s</span>" +
      "<span>T+" +
      Math.round(durSec * 0.5) +
      "s</span>" +
      "<span>T+" +
      Math.round(durSec * 0.75) +
      "s</span>" +
      "<span>T+" +
      durSec +
      "s (" +
      escapeHTML(formatHMS(win.to)) +
      ")</span>" +
      "</div>" +
      "</div>"
    );
  }

  function renderSVGLineChart(chart) {
    const W = 540;
    const H = 155;
    const padL = 42;
    const padR = 14;
    const padT = 14;
    const padB = 24;
    const plotW = W - padL - padR;
    const plotH = H - padT - padB;

    const palette = ["#38bdf8", "#f43f5e", "#10b981", "#f59e0b", "#a855f7"];
    let minT = Infinity;
    let maxT = -Infinity;
    let maxV = 0;
    let minV = Infinity;

    (chart.series || []).forEach(function (s) {
      (s.points || []).forEach(function (pt) {
        const t = new Date(pt.ts).getTime();
        if (!isNaN(t)) {
          if (t < minT) minT = t;
          if (t > maxT) maxT = t;
        }
        if (pt.value > maxV) maxV = pt.value;
        if (pt.value < minV) minV = pt.value;
      });
    });

    if (maxV <= 0) maxV = 1;
    const topV = maxV * 1.12;
    const spanT = Math.max(1000, maxT - minT);

    // Horizontal grid lines
    const gridLines = [0, 0.5, 1].map(function (frac) {
      const y = padT + plotH * (1 - frac);
      const val = (topV * frac).toFixed(topV >= 10 ? 0 : 2);
      return (
        '<line x1="' +
        padL +
        '" y1="' +
        y.toFixed(1) +
        '" x2="' +
        (W - padR) +
        '" y2="' +
        y.toFixed(1) +
        '" stroke="rgba(148,163,184,0.16)" stroke-dasharray="3 3"/>' +
        '<text x="' +
        (padL - 6) +
        '" y="' +
        (y + 4).toFixed(1) +
        '" text-anchor="end" font-size="10" fill="#8b9bb4" font-family="monospace">' +
        val +
        "</text>"
      );
    });

    const paths = [];
    const legends = [];

    (chart.series || []).slice(0, 5).forEach(function (s, idx) {
      const col = palette[idx % palette.length];
      const pts = s.points || [];
      if (pts.length === 0) return;

      const coords = pts.map(function (pt, i) {
        const t = new Date(pt.ts).getTime();
        const x =
          pts.length === 1
            ? padL + plotW / 2
            : padL + ((t - minT) / spanT) * plotW;
        const y = padT + plotH * (1 - Math.min(1, Math.max(0, pt.value / topV)));
        return (i === 0 ? "M" : "L") + x.toFixed(1) + "," + y.toFixed(1);
      });

      const lineD = coords.join(" ");
      paths.push(
        '<path d="' +
          lineD +
          '" fill="none" stroke="' +
          col +
          '" stroke-width="2.2" stroke-linecap="round" stroke-linejoin="round"/>'
      );

      let sMax = 0;
      pts.forEach(function (p) {
        if (p.value > sMax) sMax = p.value;
      });
      legends.push(
        '<span class="legend-item"><span class="dist-swatch" style="background:' +
          col +
          '"></span>' +
          escapeHTML(s.label) +
          " (peak: " +
          sMax.toFixed(sMax >= 10 ? 1 : 2) +
          " " +
          escapeHTML(chart.unit) +
          ")</span>"
      );
    });

    return (
      '<div class="chart-card">' +
      '<div class="chart-header">' +
      '<div class="chart-title">' +
      escapeHTML(chart.title) +
      "</div>" +
      '<div class="chart-stats">Peak: ' +
      maxV.toFixed(maxV >= 10 ? 1 : 2) +
      " " +
      escapeHTML(chart.unit) +
      "</div>" +
      "</div>" +
      '<div class="chart-svg-wrap">' +
      '<svg width="100%" height="100%" viewBox="0 0 ' +
      W +
      " " +
      H +
      '" preserveAspectRatio="none">' +
      gridLines.join("") +
      paths.join("") +
      "</svg>" +
      "</div>" +
      '<div class="chart-legend">' +
      legends.join("") +
      "</div>" +
      "</div>"
    );
  }

  function renderRequestLatencyChart(reqs) {
    if (!reqs || reqs.length === 0) return "";
    const W = 540;
    const H = 155;
    const padL = 44;
    const padR = 12;
    const padT = 14;
    const padB = 22;
    const plotW = W - padL - padR;
    const plotH = H - padT - padB;

    let maxLat = 100;
    let errCnt = 0;
    reqs.forEach(function (r) {
      if ((r.clientLatencyMs || 0) > maxLat) maxLat = r.clientLatencyMs;
      if (r.status >= 400 || r.status === 0 || r.error) errCnt++;
    });

    const barW = Math.max(4, Math.min(18, plotW / reqs.length - 2));
    const bars = reqs.map(function (r, idx) {
      const x =
        padL +
        (reqs.length === 1
          ? plotW / 2
          : (idx / Math.max(1, reqs.length - 1)) * (plotW - barW));
      const h = Math.max(3, ((r.clientLatencyMs || 0) / (maxLat * 1.1)) * plotH);
      const y = padT + plotH - h;
      const isErr = r.status >= 400 || r.status === 0 || r.error;
      const col = isErr ? "#f43f5e" : "#10b981";
      return (
        '<rect x="' +
        x.toFixed(1) +
        '" y="' +
        y.toFixed(1) +
        '" width="' +
        barW.toFixed(1) +
        '" height="' +
        h.toFixed(1) +
        '" rx="2" fill="' +
        col +
        '">' +
        "<title>" +
        escapeHTML(
          (r.promptId || "req") +
            " · HTTP " +
            r.status +
            " · " +
            Math.round(r.clientLatencyMs || 0) +
            " ms"
        ) +
        "</title></rect>"
      );
    });

    return (
      '<div class="chart-card">' +
      '<div class="chart-header">' +
      '<div class="chart-title">Client HTTP Request Latency &amp; Status Over Window</div>' +
      '<div class="chart-stats">Max: ' +
      Math.round(maxLat) +
      " ms · Errors: " +
      errCnt +
      "/" +
      reqs.length +
      "</div>" +
      "</div>" +
      '<div class="chart-svg-wrap">' +
      '<svg width="100%" height="100%" viewBox="0 0 ' +
      W +
      " " +
      H +
      '" preserveAspectRatio="none">' +
      '<line x1="' +
      padL +
      '" y1="' +
      padT +
      '" x2="' +
      (W - padR) +
      '" y2="' +
      padT +
      '" stroke="rgba(148,163,184,0.16)" stroke-dasharray="3 3"/>' +
      '<text x="' +
      (padL - 5) +
      '" y="' +
      (padT + 4) +
      '" text-anchor="end" font-size="10" fill="#8b9bb4" font-family="monospace">' +
      Math.round(maxLat) +
      "ms</text>" +
      bars.join("") +
      "</svg>" +
      "</div>" +
      '<div class="chart-legend">' +
      '<span class="legend-item"><span class="dist-swatch" style="background:#10b981"></span>HTTP 200 OK</span>' +
      '<span class="legend-item"><span class="dist-swatch" style="background:#f43f5e"></span>HTTP 502/504 / Timeout</span>' +
      "</div>" +
      "</div>"
    );
  }

  function renderSpanDecompositionChart(traces) {
    if (!traces || traces.length === 0) return "";
    const W = 540;
    const H = 155;
    const padL = 44;
    const padR = 12;
    const padT = 14;
    const padB = 22;
    const plotW = W - padL - padR;
    const plotH = H - padT - padB;

    let maxTot = 100;
    traces.forEach(function (t) {
      const tot = (t.preInferenceGapMs || 0) + (t.inferenceMs || 0);
      if (tot > maxTot) maxTot = tot;
    });

    const barW = Math.max(5, Math.min(20, plotW / traces.length - 3));
    const bars = traces.map(function (t, idx) {
      const x =
        padL +
        (traces.length === 1
          ? plotW / 2
          : (idx / Math.max(1, traces.length - 1)) * (plotW - barW));
      const gapH = ((t.preInferenceGapMs || 0) / (maxTot * 1.1)) * plotH;
      const infH = ((t.inferenceMs || 0) / (maxTot * 1.1)) * plotH;
      const yInf = padT + plotH - infH;
      const yGap = yInf - gapH;
      return (
        '<g><title>Trace ' +
        escapeHTML((t.traceId || "").slice(0, 12)) +
        " · Pre-Inference Gap: " +
        Math.round(t.preInferenceGapMs || 0) +
        "ms · LLM Inference: " +
        Math.round(t.inferenceMs || 0) +
        "ms</title>" +
        '<rect x="' +
        x.toFixed(1) +
        '" y="' +
        yInf.toFixed(1) +
        '" width="' +
        barW.toFixed(1) +
        '" height="' +
        Math.max(2, infH).toFixed(1) +
        '" fill="#38bdf8"/>' +
        '<rect x="' +
        x.toFixed(1) +
        '" y="' +
        yGap.toFixed(1) +
        '" width="' +
        barW.toFixed(1) +
        '" height="' +
        Math.max(0, gapH).toFixed(1) +
        '" fill="#f59e0b"/>' +
        "</g>"
      );
    });

    return (
      '<div class="chart-card">' +
      '<div class="chart-header">' +
      '<div class="chart-title">OTel Span Decomposition (Pre-Inference Gap vs LLM Inference)</div>' +
      '<div class="chart-stats">' +
      traces.length +
      " traces</div>" +
      "</div>" +
      '<div class="chart-svg-wrap">' +
      '<svg width="100%" height="100%" viewBox="0 0 ' +
      W +
      " " +
      H +
      '" preserveAspectRatio="none">' +
      bars.join("") +
      "</svg>" +
      "</div>" +
      '<div class="chart-legend">' +
      '<span class="legend-item"><span class="dist-swatch" style="background:#f59e0b"></span>Pre-Inference App Gap (chat-api)</span>' +
      '<span class="legend-item"><span class="dist-swatch" style="background:#38bdf8"></span>Model Inference Span (model-server)</span>' +
      "</div>" +
      "</div>"
    );
  }

  function buildChartsTabHTML(inc) {
    const tel = inc.telemetry || {};
    const cards = [];
    const reqChart = renderRequestLatencyChart(tel.requests || []);
    if (reqChart) cards.push(reqChart);
    const spanChart = renderSpanDecompositionChart(tel.traces || []);
    if (spanChart) cards.push(spanChart);
    (tel.charts || []).forEach(function (ch) {
      cards.push(renderSVGLineChart(ch));
    });
    if (cards.length === 0) {
      return '<div class="empty-state">No time-series charts available for this snapshot.</div>';
    }
    return '<div class="charts-grid">' + cards.join("") + "</div>";
  }

  function buildTracesTabHTML(inc) {
    const tel = inc.telemetry || {};
    const traces = tel.traces || [];
    const reqs = tel.requests || [];

    if (traces.length === 0 && reqs.length === 0) {
      return '<div class="empty-state">No traces or request samples recorded in this window.</div>';
    }

    let maxDur = 100;
    traces.forEach(function (t) {
      const tot = Math.max(t.httpDurationMs || 0, (t.preInferenceGapMs || 0) + (t.inferenceMs || 0));
      if (tot > maxDur) maxDur = tot;
    });

    const traceRows = traces
      .map(function (t) {
        const gapMs = Math.round(t.preInferenceGapMs || 0);
        const infMs = Math.round(t.inferenceMs || 0);
        const totMs = Math.round(t.httpDurationMs || gapMs + infMs);
        const gapPct = Math.min(100, (gapMs / maxDur) * 100);
        const infPct = Math.min(100 - gapPct, (infMs / maxDur) * 100);
        const infClass = t.error ? "wf-err" : "wf-inf";
        return (
          "<tr>" +
          "<td><code>" +
          escapeHTML((t.traceId || "").slice(0, 16)) +
          "...</code></td>" +
          "<td><code>" +
          escapeHTML(formatHMS(t.startTime)) +
          "</code></td>" +
          "<td>" +
          escapeHTML(t.clientPodName || "chat-api") +
          "</td>" +
          "<td>" +
          '<div class="waterfall-bar-wrap" title="Pre-Inference Gap: ' +
          gapMs +
          "ms | LLM Inference: " +
          infMs +
          'ms">' +
          '<div class="wf-gap" style="width:' +
          gapPct.toFixed(1) +
          '%"></div>' +
          '<div class="' +
          infClass +
          '" style="width:' +
          infPct.toFixed(1) +
          '%"></div>' +
          "</div>" +
          "</td>" +
          "<td><strong>" +
          gapMs +
          " ms</strong></td>" +
          "<td><strong>" +
          infMs +
          " ms</strong></td>" +
          "<td>" +
          totMs +
          " ms</td>" +
          "<td>" +
          escapeHTML((t.inputTokens || 0) + " → " + (t.outputTokens || 0)) +
          "</td>" +
          '<td><span class="status-tag ' +
          (t.error ? "status-diagnosed" : "status-unknown") +
          '">' +
          escapeHTML(t.error ? "ERR " + (t.httpStatusCode || "") : "200 OK") +
          "</span></td>" +
          "</tr>"
        );
      })
      .join("");

    return (
      '<div class="section-card">' +
      '<div class="section-card-header">' +
      '<h3 class="section-card-title">OpenTelemetry GenAI Trace Waterfall (Privacy-Safe · Zero Prompt Content)</h3>' +
      '<div class="chart-legend" style="margin:0">' +
      '<span class="legend-item"><span class="dist-swatch" style="background:var(--amber)"></span>Pre-Inference App Gap</span>' +
      '<span class="legend-item"><span class="dist-swatch" style="background:var(--accent)"></span>LLM Inference Span</span>' +
      "</div>" +
      "</div>" +
      '<table class="evidence-table">' +
      "<thead><tr>" +
      "<th>Trace ID</th><th>Start (UTC)</th><th>Client Pod</th><th>Span Waterfall</th><th>App Gap</th><th>LLM Span</th><th>Total</th><th>Tokens</th><th>Outcome</th>" +
      "</tr></thead>" +
      "<tbody>" +
      (traceRows ||
        '<tr><td colspan="9" class="empty-state">No OTel traces exported (requests failed before trace emission).</td></tr>') +
      "</tbody></table>" +
      "</div>"
    );
  }

  function buildPodsAndEventsTabHTML(inc) {
    const tel = inc.telemetry || {};
    const pods = tel.pods || [];
    const events = tel.events || [];

    const podCards = pods
      .map(function (p) {
        const conts = (p.containers || []).concat(p.initContainers || []);
        const contHTML = conts
          .map(function (c) {
            const cpuLim = c.limitsCpuMillis ? c.limitsCpuMillis + "m" : "none";
            const memLim = c.limitsMemoryBytes
              ? Math.round(c.limitsMemoryBytes / (1024 * 1024)) + "Mi"
              : "none";
            const lastTerm = c.lastTerminated
              ? '<div class="alert-error" style="margin-top:0.35rem;padding:0.3rem 0.55rem;font-size:0.73rem">Last Terminated: <strong>' +
                escapeHTML(c.lastTerminated.reason || "Terminated") +
                " (exit " +
                escapeHTML(c.lastTerminated.exitCode) +
                ")</strong></div>"
              : "";
            const probeInfo = c.readinessProbe
              ? '<div style="font-size:0.71rem;color:var(--muted);margin-top:0.25rem">ReadinessProbe: <code>' +
                escapeHTML(
                  (c.readinessProbe.path || "/") +
                    ":" +
                    (c.readinessProbe.port || "") +
                    " (initialDelay=" +
                    (c.readinessProbe.initialDelaySeconds || 0) +
                    "s)"
                ) +
                "</code></div>"
              : "";
            return (
              '<div style="border-top:1px solid var(--border-subtle);padding-top:0.45rem;margin-top:0.45rem;font-size:0.75rem">' +
              '<div style="display:flex;justify-content:space-between">' +
              "<strong>" +
              escapeHTML((c.isInit ? "[init] " : "") + c.name) +
              "</strong>" +
              "<span>restarts: <strong>" +
              escapeHTML(c.restartCount || 0) +
              "</strong> · ready: <strong>" +
              escapeHTML(c.ready ? "true" : "false") +
              "</strong></span>" +
              "</div>" +
              '<div style="color:var(--muted);margin-top:0.2rem">Limits: CPU <code>' +
              escapeHTML(cpuLim) +
              "</code> · Memory <code>" +
              escapeHTML(memLim) +
              "</code> · State: <code>" +
              escapeHTML((c.state && c.state.kind) || "-") +
              (c.state && c.state.reason ? " (" + c.state.reason + ")" : "") +
              "</code></div>" +
              probeInfo +
              lastTerm +
              "</div>"
            );
          })
          .join("");

        return (
          '<div class="pod-card">' +
          '<div class="pod-card-top">' +
          '<span class="pod-name">' +
          escapeHTML(p.name) +
          "</span>" +
          '<span class="status-tag ' +
          (p.phase === "Running" ? "status-unknown" : "status-diagnosed") +
          '">' +
          escapeHTML(p.phase || "Unknown") +
          "</span>" +
          "</div>" +
          '<div style="font-size:0.73rem;color:var(--muted)">Namespace: <code>' +
          escapeHTML(p.namespace) +
          "</code> · Node: <code>" +
          escapeHTML(p.nodeName || "unassigned") +
          "</code></div>" +
          contHTML +
          "</div>"
        );
      })
      .join("");

    const evRows = events
      .map(function (e) {
        const badgeClass = e.type === "Warning" ? "src-request_log" : "src-k8s_event";
        return (
          "<tr>" +
          '<td><span class="source-badge ' +
          badgeClass +
          '">' +
          escapeHTML(e.type || "Normal") +
          "</span></td>" +
          "<td><code>" +
          escapeHTML(formatShortTime(e.timestamp)) +
          "</code></td>" +
          "<td><strong>" +
          escapeHTML(e.reason) +
          "</strong></td>" +
          "<td><code>" +
          escapeHTML(e.involvedKind + "/" + e.involvedName) +
          "</code></td>" +
          "<td>" +
          escapeHTML(e.message) +
          "</td>" +
          "</tr>"
        );
      })
      .join("");

    return (
      '<div class="section-card">' +
      '<div class="section-card-header"><h3 class="section-card-title">Kubernetes Pod Specs, Resource Limits &amp; Container States</h3></div>' +
      '<div class="pods-grid">' +
      (podCards || '<div class="empty-state">No pods captured.</div>') +
      "</div>" +
      "</div>" +
      '<div class="section-card">' +
      '<div class="section-card-header"><h3 class="section-card-title">Kubernetes Events in Analysis Window</h3></div>' +
      '<table class="evidence-table">' +
      "<thead><tr><th>Type</th><th>Timestamp</th><th>Reason</th><th>Object</th><th>Message</th></tr></thead>" +
      "<tbody>" +
      (evRows || '<tr><td colspan="5" class="empty-state">No Kubernetes events in window.</td></tr>') +
      "</tbody></table>" +
      "</div>"
    );
  }

  function buildExportTabHTML(inc) {
    const rep = inc.report || {};
    const win = rep.window || {};
    const cliCmd = inc.runId
      ? "./bin/why-slow --run-dir eval/runs/dev/" + inc.runId + " --format text"
      : './bin/why-slow --namespace ' +
        (inc.namespace || "genai") +
        ' --from "' +
        (win.from || "") +
        '" --to "' +
        (win.to || "") +
        '"';

    const top =
      rep.topCauses && rep.topCauses.length > 0 ? rep.topCauses[0] : null;
    const crdYAML =
      "apiVersion: kubepilot.io/v1alpha1\n" +
      "kind: GenAIIncident\n" +
      "metadata:\n" +
      "  name: " +
      (inc.id || "incident").toLowerCase().replace(/[^a-z0-9.-]/g, "-") +
      "\n" +
      "  namespace: " +
      (inc.namespace || "genai") +
      "\n" +
      "spec:\n" +
      "  source: " +
      (inc.source || "run-dir") +
      "\n" +
      "  window:\n" +
      '    from: "' +
      (win.from || "") +
      '"\n' +
      '    to: "' +
      (win.to || "") +
      '"\n' +
      "status:\n" +
      "  diagnosisStatus: " +
      (rep.status || "unknown") +
      "\n" +
      (top
        ? "  topCause: " +
          top.cause +
          "\n  confidence: " +
          top.confidence +
          "\n  summary: " +
          JSON.stringify(top.summary) +
          "\n"
        : "");

    const cleanJSON = JSON.stringify(
      {
        id: inc.id,
        namespace: inc.namespace,
        report: inc.report,
        cost: inc.cost,
      },
      null,
      2
    );

    return (
      '<div class="section-card">' +
      '<div class="section-card-header"><h3 class="section-card-title">Reproduce via CLI (kubectl why-slow)</h3></div>' +
      '<pre class="code-block">' +
      escapeHTML(cliCmd) +
      "</pre>" +
      "</div>" +
      '<div class="section-card">' +
      '<div class="section-card-header"><h3 class="section-card-title">Read-Only GenAIIncident Custom Resource (kubepilot.io/v1alpha1)</h3></div>' +
      '<pre class="code-block">' +
      escapeHTML(crdYAML) +
      "</pre>" +
      "</div>" +
      '<div class="section-card">' +
      '<div class="section-card-header"><h3 class="section-card-title">Structured Report JSON</h3></div>' +
      '<pre class="code-block">' +
      escapeHTML(cleanJSON) +
      "</pre>" +
      "</div>"
    );
  }

  function renderIncidentDetail(inc) {
    const rep = inc.report || {};
    const win = rep.window || {};
    const causes = rep.topCauses || [];
    const isDiagnosed = rep.status === "diagnosed" && causes.length > 0;
    const topCause = isDiagnosed ? causes[0] : null;
    const topMeta = topCause ? getCauseMeta(topCause.cause) : getCauseMeta("");
    const tel = inc.telemetry || {};

    const winStartMs = win.from ? new Date(win.from).getTime() : 0;
    const winEndMs = win.to ? new Date(win.to).getTime() : 0;
    const winSec =
      winStartMs && winEndMs && winEndMs > winStartMs
        ? (winEndMs - winStartMs) / 1000
        : inc.cost
        ? inc.cost.windowDurationSec
        : 0;

    const topPct = topCause ? Math.round((topCause.confidence || 0) * 100) : 100;

    // 1. Plain-English Executive Banner + Radial Gauge
    const bannerClass = isDiagnosed ? "banner-diagnosed" : "banner-healthy";
    const bannerHTML =
      '<div class="detail-banner ' +
      bannerClass +
      '">' +
      "<div>" +
      '<div class="banner-eyebrow">' +
      escapeHTML(
        isDiagnosed
          ? "Root Cause Isolated · " + topMeta.layer
          : "Healthy Window · Abstention Guard (0 False Positives)"
      ) +
      "</div>" +
      '<h2 class="banner-title">' +
      escapeHTML(topMeta.title) +
      "</h2>" +
      '<p class="banner-explain">' +
      escapeHTML(topMeta.explain) +
      "</p>" +
      "</div>" +
      renderRadialGaugeSVG(topPct, isDiagnosed) +
      "</div>";

    // 2. Metadata Strip
    const metaGridHTML =
      '<div class="meta-grid">' +
      '<div class="meta-box"><div class="label">Incident / Run ID</div><div class="value">' +
      escapeHTML(inc.id) +
      "</div></div>" +
      '<div class="meta-box"><div class="label">Window Duration</div><div class="value">' +
      escapeHTML(formatDurationSec(winSec)) +
      "</div></div>" +
      '<div class="meta-box"><div class="label">Correlated Signals</div><div class="value">' +
      escapeHTML(inc.traceCount || 0) +
      " traces · " +
      escapeHTML(inc.eventCount || 0) +
      " events · " +
      escapeHTML(inc.podCount || 0) +
      " pods</div></div>" +
      '<div class="meta-box"><div class="label">Window (UTC)</div><div class="value">' +
      escapeHTML(formatShortTime(win.from)) +
      " &rarr; " +
      escapeHTML(formatShortTime(win.to)) +
      "</div></div>" +
      "</div>";

    // 3. Probable Causes & Correlated Proof Cards
    let causesHTML = "";
    if (!isDiagnosed) {
      causesHTML =
        '<div class="cause-card">' +
        '<div class="cause-header"><span class="cause-rank">No Infrastructure or Application Fault Detected</span>' +
        '<span class="confidence-pill">0 False Positives</span></div>' +
        '<p class="cause-summary">' +
        escapeHTML(
          rep.reason ||
            "All 8 deterministic rules evaluated the telemetry snapshot and abstained because GenAI serving metrics and Kubernetes state in namespace 'genai' remained healthy."
        ) +
        "</p>" +
        "</div>";
    } else {
      causesHTML = causes
        .map(function (c, idx) {
          const cmeta = getCauseMeta(c.cause);
          const comp = c.component || {};
          const pct = Math.round((c.confidence || 0) * 100);
          const evs = c.evidence || [];
          const evRows = evs
            .map(function (ev) {
              const srcClass =
                "src-" + String(ev.source || "").replace(/[^a-z0-9_]/gi, "");
              let offsetBadge = "";
              if (ev.timestamp && winStartMs) {
                const evMs = new Date(ev.timestamp).getTime();
                if (!isNaN(evMs)) {
                  const diffSec = Math.round((evMs - winStartMs) / 1000);
                  offsetBadge =
                    ' <span class="pill">' +
                    (diffSec >= 0 ? "T+" + diffSec + "s" : "T" + diffSec + "s") +
                    "</span>";
                }
              }
              return (
                "<tr>" +
                '<td><span class="source-badge ' +
                srcClass +
                '">' +
                escapeHTML(ev.source) +
                "</span></td>" +
                "<td><code>" +
                escapeHTML(formatShortTime(ev.timestamp)) +
                "</code>" +
                offsetBadge +
                "</td>" +
                "<td>" +
                escapeHTML(ev.summary) +
                "</td>" +
                "</tr>"
              );
            })
            .join("");

          return (
            '<div class="cause-card">' +
            '<div class="cause-header">' +
            "<div>" +
            '<span class="cause-rank">#' +
            (idx + 1) +
            " " +
            escapeHTML(cmeta.title) +
            "</span>" +
            '<span class="cause-code">' +
            escapeHTML(c.cause) +
            "</span>" +
            "</div>" +
            '<div class="confidence-wrap">' +
            '<div class="progress-bar"><div class="progress-fill" style="width:' +
            pct +
            '%"></div></div>' +
            '<span class="confidence-pill">' +
            pct +
            "% confidence</span>" +
            "</div>" +
            "</div>" +
            '<div class="component-chips">' +
            '<span class="comp-chip">Namespace: ' +
            escapeHTML(comp.namespace || "genai") +
            "</span>" +
            '<span class="comp-chip">Workload: ' +
            escapeHTML(comp.workload || "-") +
            "</span>" +
            (comp.pod
              ? '<span class="comp-chip">Pod: ' + escapeHTML(comp.pod) + "</span>"
              : "") +
            (comp.node
              ? '<span class="comp-chip">Node: ' + escapeHTML(comp.node) + "</span>"
              : "") +
            '<span class="comp-chip">Rule: ' +
            escapeHTML(c.rule) +
            "</span>" +
            "</div>" +
            '<div class="cause-summary"><strong>Measured Finding:</strong> ' +
            escapeHTML(c.summary) +
            "</div>" +
            '<div class="remediation-box"><strong>How to verify / fix (Read-Only Guidance):</strong> ' +
            escapeHTML(cmeta.verify) +
            "</div>" +
            '<table class="evidence-table">' +
            '<thead><tr><th style="width:130px">Signal Source</th><th style="width:240px">Timestamp &amp; Window Offset</th><th>Correlated Proof</th></tr></thead>' +
            "<tbody>" +
            evRows +
            "</tbody></table>" +
            "</div>"
          );
        })
        .join("");
    }

    // 4. OpenCost Card
    let costHTML = "";
    if (inc.cost) {
      const totalC = Number(inc.cost.totalWindowCost || 0);
      const wasteC = Number(inc.cost.estimatedWasteCost || 0);
      const wastePct =
        totalC > 0 ? Math.min(100, Math.round((wasteC / totalC) * 100)) : 0;
      costHTML =
        '<div class="cause-card">' +
        '<div class="cause-header">' +
        '<span class="cause-rank">OpenCost Incident Resource &amp; Waste Estimate</span>' +
        '<div class="confidence-wrap">' +
        '<div class="progress-bar"><div class="progress-fill" style="width:' +
        wastePct +
        '%"></div></div>' +
        '<span class="confidence-pill">' +
        wastePct +
        "% waste ratio (" +
        escapeHTML(inc.cost.source || "opencost") +
        ")</span></div></div>" +
        '<div class="meta-grid">' +
        '<div class="meta-box"><div class="label">Total Window Allocation</div><div class="value">$' +
        totalC.toFixed(4) +
        " " +
        escapeHTML(inc.cost.currency || "USD") +
        "</div></div>" +
        '<div class="meta-box"><div class="label">Estimated Incident Waste</div><div class="value">$' +
        wasteC.toFixed(4) +
        " " +
        escapeHTML(inc.cost.currency || "USD") +
        "</div></div>" +
        '<div class="meta-box"><div class="label">CPU vs RAM Split</div><div class="value">CPU $' +
        Number(inc.cost.cpuCost || 0).toFixed(4) +
        " · RAM $" +
        Number(inc.cost.ramCost || 0).toFixed(4) +
        "</div></div>" +
        "</div>" +
        '<div class="remediation-box">' +
        escapeHTML(inc.cost.explanation || "") +
        "</div>" +
        "</div>";
    }

    const chartCount =
      ((tel.charts && tel.charts.length) || 0) +
      ((tel.requests && tel.requests.length > 0) ? 1 : 0) +
      ((tel.traces && tel.traces.length > 0) ? 1 : 0);

    const tabsNavHTML =
      '<div class="tabs-bar" role="tablist">' +
      '<button type="button" class="tab-btn' +
      (activeTab === "overview" ? " active" : "") +
      '" data-tab="overview">1. Overview &amp; Topology</button>' +
      '<button type="button" class="tab-btn' +
      (activeTab === "charts" ? " active" : "") +
      '" data-tab="charts">2. Metrics &amp; Latency Charts <span class="pill">' +
      chartCount +
      "</span></button>" +
      '<button type="button" class="tab-btn' +
      (activeTab === "traces" ? " active" : "") +
      '" data-tab="traces">3. GenAI Traces Waterfall <span class="pill">' +
      (inc.traceCount || 0) +
      "</span></button>" +
      '<button type="button" class="tab-btn' +
      (activeTab === "k8s" ? " active" : "") +
      '" data-tab="k8s">4. Kubernetes Pods &amp; Events <span class="pill">' +
      ((inc.podCount || 0) + (inc.eventCount || 0)) +
      "</span></button>" +
      '<button type="button" class="tab-btn' +
      (activeTab === "export" ? " active" : "") +
      '" data-tab="export">5. CLI, CRD &amp; JSON</button>' +
      "</div>";

    const overviewPanel =
      '<div class="tab-panel' +
      (activeTab === "overview" ? " active" : "") +
      '" data-panel="overview">' +
      buildTopologyHTML(inc, topCause, topMeta) +
      buildSwimlaneHTML(inc) +
      causesHTML +
      costHTML +
      "</div>";

    const chartsPanel =
      '<div class="tab-panel' +
      (activeTab === "charts" ? " active" : "") +
      '" data-panel="charts">' +
      buildChartsTabHTML(inc) +
      "</div>";

    const tracesPanel =
      '<div class="tab-panel' +
      (activeTab === "traces" ? " active" : "") +
      '" data-panel="traces">' +
      buildTracesTabHTML(inc) +
      "</div>";

    const k8sPanel =
      '<div class="tab-panel' +
      (activeTab === "k8s" ? " active" : "") +
      '" data-panel="k8s">' +
      buildPodsAndEventsTabHTML(inc) +
      "</div>";

    const exportPanel =
      '<div class="tab-panel' +
      (activeTab === "export" ? " active" : "") +
      '" data-panel="export">' +
      buildExportTabHTML(inc) +
      "</div>";

    detailEl.innerHTML =
      bannerHTML +
      metaGridHTML +
      tabsNavHTML +
      overviewPanel +
      chartsPanel +
      tracesPanel +
      k8sPanel +
      exportPanel;

    Array.from(detailEl.querySelectorAll(".tab-btn")).forEach(function (btn) {
      btn.addEventListener("click", function () {
        activeTab = btn.getAttribute("data-tab") || "overview";
        Array.from(detailEl.querySelectorAll(".tab-btn")).forEach(function (b) {
          b.classList.toggle("active", b.getAttribute("data-tab") === activeTab);
        });
        Array.from(detailEl.querySelectorAll(".tab-panel")).forEach(function (p) {
          p.classList.toggle("active", p.getAttribute("data-panel") === activeTab);
        });
      });
    });
  }

  async function runDiagnose(payload) {
    errEl.classList.add("hidden");
    try {
      const resp = await fetch("/api/v1/diagnose", {
        method: "POST",
        headers: { "Content-Type": "application/json" },
        body: JSON.stringify(payload),
      });
      const body = await resp.json();
      if (!resp.ok) {
        throw new Error(body.error || "HTTP " + resp.status);
      }
      activeID = body.id;
      await fetchIncidents();
      renderIncidentDetail(body);
    } catch (e) {
      errEl.textContent = "Diagnosis error: " + e.message;
      errEl.classList.remove("hidden");
    }
  }

  form.addEventListener("submit", function (ev) {
    ev.preventDefault();
    runDiagnose({
      traceId: document.getElementById("input-trace-id").value.trim(),
      runDir: document.getElementById("input-run-dir").value.trim(),
      from: document.getElementById("input-from").value.trim(),
      to: document.getElementById("input-to").value.trim(),
    });
  });

  Array.from(document.querySelectorAll(".chip-btn")).forEach(function (btn) {
    btn.addEventListener("click", function () {
      const preset = btn.getAttribute("data-preset") || "";
      document.getElementById("input-run-dir").value = preset;
      document.getElementById("input-trace-id").value = "";
      document.getElementById("input-from").value = "";
      document.getElementById("input-to").value = "";
      runDiagnose({ runDir: preset });
    });
  });

  filterStatus.addEventListener("change", function () {
    activeID = null;
    fetchIncidents();
  });
  filterCause.addEventListener("change", function () {
    activeID = null;
    fetchIncidents();
  });
  filterSearch.addEventListener("input", applySearchAndRender);
  btnRefresh.addEventListener("click", fetchIncidents);

  fetchIncidents().catch(function (e) {
    listEl.innerHTML =
      '<li class="empty-state">Error loading incidents: ' +
      escapeHTML(e.message) +
      "</li>";
  });
})();
