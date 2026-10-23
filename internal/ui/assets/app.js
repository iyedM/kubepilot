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

  const kpiTotal = document.getElementById("kpi-total");
  const kpiDiagnosed = document.getElementById("kpi-diagnosed");
  const kpiHealthy = document.getElementById("kpi-healthy");
  const kpiWaste = document.getElementById("kpi-waste");

  let activeID = null;
  let allIncidents = [];

  const CAUSE_META = {
    cpu_throttling: {
      title: "CPU Throttling (CFS Quota Saturation)",
      layer: "Container Cgroup CPU Limit",
      explain:
        "The container's Linux CFS CPU quota limit is too low for inference demand, stalling threads across a high fraction of 100ms periods and inflating token generation latency.",
      verify:
        "Inspect resources.limits.cpu on the workload Deployment and compare against container_cpu_cfs_throttled_periods_total in Prometheus.",
    },
    oom_killed: {
      title: "Container Out-of-Memory (OOMKilled · Exit 137)",
      layer: "Container Memory Limit / KV Cache",
      explain:
        "The Linux kernel OOM killer terminated the container (exit code 137) because model weights, KV cache, or application memory exceeded resources.limits.memory.",
      verify:
        "Check pod lastState.terminated.reason == OOMKilled and increase resources.limits.memory or reduce model context/batch size.",
    },
    unschedulable: {
      title: "Pod Unschedulable (Pending Scheduling)",
      layer: "Kubernetes Scheduler / Node Capacity / Taints / PVC",
      explain:
        "Kubernetes could not place the pod onto any worker node due to insufficient CPU/memory, unmatched nodeSelector/affinity, untolerated node taints, or an unbound PersistentVolumeClaim.",
      verify:
        "Inspect Warning FailedScheduling events on the Pending pod and check node taints, selectors, or PVC binding status.",
    },
    readiness_failing: {
      title: "Failing or Delayed Readiness Probe",
      layer: "Kubernetes Kubelet Probe / Service Endpoint",
      explain:
        "The pod is Running, but its readinessProbe is failing (HTTP 4xx/5xx, connection refused on wrong port) or delayed by an excessive initialDelaySeconds, keeping it out of Service endpoints.",
      verify:
        "Check Warning Unhealthy events and verify readinessProbe.httpGet.path, port, and initialDelaySeconds in the Pod spec.",
    },
    cold_start: {
      title: "Slow Model Cold Start (Init / Image Pull Delay)",
      layer: "Pod Initialization / Model Weight Fetch",
      explain:
        "Traffic failed or stalled during a rollout because an initContainer (model weight download) or uncached container image pull delayed main container startup before the pod became Ready.",
      verify:
        "Compare initContainerStatuses[].state.terminated.{startedAt,finishedAt} and pre-cache model weights on a persistent volume or node image cache.",
    },
    config_change: {
      title: "Serving Configuration Regression (Rollout)",
      layer: "Model Server Runtime Flags",
      explain:
        "A new Deployment rollout changed model-server runtime arguments (e.g. --threads 1), reducing token generation speed (llamacpp:predicted_tokens_seconds) without saturating CPU or memory.",
      verify:
        "Diff container args/env between the previous and current ReplicaSet pods and inspect llamacpp:predicted_tokens_seconds.",
    },
    host_cpu_contention: {
      title: "Host CPU Contention (Noisy Neighbor on Node)",
      layer: "Worker Node Kernel Scheduler / Linux PSI",
      explain:
        "The pod is not hitting its own CFS CPU limit, but threads are waiting on the host CPU runqueue (high container_pressure_cpu_waiting_seconds_total) due to another workload or process saturating the same node.",
      verify:
        "Check Linux PSI CPU waiting metrics and top CPU-consuming pods on the same worker node.",
    },
    unknown_no_infrastructure_cause: {
      title: "Application Pre-Inference Delay (Code / Hook Inside App)",
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
        title: cause || "Healthy / No Deterministic Fault Matched",
        layer: "Abstention (No Fault Detected)",
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
    updateKPIs(allIncidents);
    applySearchAndRender();
  }

  function updateKPIs(incidents) {
    let diag = 0;
    let healthy = 0;
    let waste = 0;
    incidents.forEach(function (inc) {
      const st = inc.report && inc.report.status;
      if (st === "diagnosed") {
        diag++;
      } else {
        healthy++;
      }
      if (inc.cost && inc.cost.estimatedWasteCost) {
        waste += Number(inc.cost.estimatedWasteCost) || 0;
      }
    });
    kpiTotal.textContent = String(incidents.length);
    kpiDiagnosed.textContent = String(diag);
    kpiHealthy.textContent = String(healthy);
    kpiWaste.textContent = "$" + waste.toFixed(4);
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
        const humanTitle = top
          ? meta.title
          : "Healthy Window (No Fault Blamed)";
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

  function renderIncidentDetail(inc) {
    const rep = inc.report || {};
    const win = rep.window || {};
    const causes = rep.topCauses || [];
    const isDiagnosed = rep.status === "diagnosed" && causes.length > 0;
    const topCause = isDiagnosed ? causes[0] : null;
    const topMeta = topCause ? getCauseMeta(topCause.cause) : getCauseMeta("");

    const winStartMs = win.from ? new Date(win.from).getTime() : 0;
    const winEndMs = win.to ? new Date(win.to).getTime() : 0;
    const winSec =
      winStartMs && winEndMs && winEndMs > winStartMs
        ? (winEndMs - winStartMs) / 1000
        : inc.cost
        ? inc.cost.windowDurationSec
        : 0;

    // 1. Plain-English Executive Banner
    const bannerClass = isDiagnosed ? "banner-diagnosed" : "banner-healthy";
    const bannerHTML =
      '<div class="detail-banner ' +
      bannerClass +
      '">' +
      "<div>" +
      '<div class="banner-eyebrow">' +
      escapeHTML(isDiagnosed ? "Root Cause Isolated · " + topMeta.layer : "Healthy Window · Abstention Guard") +
      "</div>" +
      '<h2 class="banner-title">' +
      escapeHTML(topMeta.title) +
      "</h2>" +
      '<p class="banner-explain">' +
      escapeHTML(topMeta.explain) +
      "</p>" +
      "</div>" +
      '<div class="confidence-wrap">' +
      '<span class="status-tag ' +
      (isDiagnosed ? "status-diagnosed" : "status-unknown") +
      '">' +
      escapeHTML(isDiagnosed ? "DIAGNOSED" : "HEALTHY / UNKNOWN") +
      "</span>" +
      "</div>" +
      "</div>";

    // 2. Metadata Grid
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

    // 3. Probable Causes & Correlated Evidence Cards
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
              const srcClass = "src-" + String(ev.source || "").replace(/[^a-z0-9_]/gi, "");
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
            "<thead><tr><th style=\"width:130px\">Signal Source</th><th style=\"width:240px\">Timestamp &amp; Window Offset</th><th>Correlated Proof</th></tr></thead>" +
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
      const wastePct = totalC > 0 ? Math.min(100, Math.round((wasteC / totalC) * 100)) : 0;
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

    detailEl.innerHTML = bannerHTML + metaGridHTML + causesHTML + costHTML;
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

  filterStatus.addEventListener("change", fetchIncidents);
  filterCause.addEventListener("change", fetchIncidents);
  filterSearch.addEventListener("input", applySearchAndRender);
  btnRefresh.addEventListener("click", fetchIncidents);

  fetchIncidents().catch(function (e) {
    listEl.innerHTML =
      '<li class="empty-state">Error loading incidents: ' +
      escapeHTML(e.message) +
      "</li>";
  });
})();
