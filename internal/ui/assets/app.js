(function () {
  "use strict";

  const listEl = document.getElementById("incident-list");
  const countEl = document.getElementById("incident-count");
  const detailEl = document.getElementById("detail-pane");
  const filterStatus = document.getElementById("filter-status");
  const filterCause = document.getElementById("filter-cause");
  const form = document.getElementById("diagnose-form");
  const errEl = document.getElementById("diagnose-error");
  const btnRefresh = document.getElementById("btn-refresh");

  let activeID = null;

  function escapeHTML(str) {
    return String(str == null ? "" : str)
      .replace(/&/g, "&amp;")
      .replace(/</g, "&lt;")
      .replace(/>/g, "&gt;")
      .replace(/"/g, "&quot;");
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
    const incidents = data.incidents || [];
    countEl.textContent = String(incidents.length);
    renderIncidentList(incidents);
    if (incidents.length > 0 && !activeID) {
      selectIncident(incidents[0].id);
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
        const causeLabel = top
          ? top.cause + " (" + Math.round(top.confidence * 100) + "%)"
          : "unknown";
        const activeClass = inc.id === activeID ? " active" : "";
        const statusClass =
          status === "diagnosed" ? "status-diagnosed" : "status-unknown";
        const costBadge = inc.cost
          ? " · waste $" + Number(inc.cost.estimatedWasteCost || 0).toFixed(4)
          : "";
        return (
          '<li class="incident-item' +
          activeClass +
          '" data-id="' +
          escapeHTML(inc.id) +
          '">' +
          '<div class="incident-item-top">' +
          '<span class="incident-id">' +
          escapeHTML(inc.id) +
          "</span>" +
          '<span class="status-tag ' +
          statusClass +
          '">' +
          escapeHTML(status) +
          "</span>" +
          "</div>" +
          '<div class="cause-title">' +
          escapeHTML(causeLabel) +
          "</div>" +
          '<div class="meta-row">' +
          "<span>ns=" +
          escapeHTML(inc.namespace || "genai") +
          costBadge +
          "</span>" +
          "<span>traces=" +
          escapeHTML(inc.traceCount || 0) +
          " events=" +
          escapeHTML(inc.eventCount || 0) +
          "</span>" +
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

    let costHTML = "";
    if (inc.cost) {
      costHTML =
        '<div class="cause-card">' +
        '<div class="cause-header"><span class="cause-rank">OpenCost Incident Resource Estimate</span>' +
        '<span class="confidence-pill">Source: ' +
        escapeHTML(inc.cost.source || "opencost") +
        "</span></div>" +
        '<div class="meta-grid">' +
        '<div class="meta-box"><div class="label">Total Window Cost</div><div class="value">$' +
        Number(inc.cost.totalWindowCost || 0).toFixed(4) +
        " " +
        escapeHTML(inc.cost.currency || "USD") +
        "</div></div>" +
        '<div class="meta-box"><div class="label">Estimated Incident Waste</div><div class="value">$' +
        Number(inc.cost.estimatedWasteCost || 0).toFixed(4) +
        " " +
        escapeHTML(inc.cost.currency || "USD") +
        "</div></div>" +
        '<div class="meta-box"><div class="label">CPU / RAM Breakdown</div><div class="value">CPU $' +
        Number(inc.cost.cpuCost || 0).toFixed(4) +
        " / RAM $" +
        Number(inc.cost.ramCost || 0).toFixed(4) +
        "</div></div>" +
        "</div>" +
        '<p class="cause-summary">' +
        escapeHTML(inc.cost.explanation || "") +
        "</p>" +
        "</div>";
    }

    let causesHTML = "";
    if (causes.length === 0) {
      causesHTML =
        '<div class="cause-card"><strong>Status: ' +
        escapeHTML(rep.status || "unknown") +
        "</strong><p class=\"cause-summary\">" +
        escapeHTML(
          rep.reason || "No deterministic rule matched the snapshot evidence."
        ) +
        "</p></div>";
    } else {
      causesHTML = causes
        .map(function (c, idx) {
          const comp = c.component || {};
          const evs = c.evidence || [];
          const evRows = evs
            .map(function (ev) {
              return (
                "<tr>" +
                '<td><span class="source-badge">' +
                escapeHTML(ev.source) +
                "</span></td>" +
                "<td><code>" +
                escapeHTML(ev.timestamp || "-") +
                "</code></td>" +
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
            '<span class="cause-rank">#' +
            (idx + 1) +
            " " +
            escapeHTML(c.cause) +
            " (rule: " +
            escapeHTML(c.rule) +
            ")</span>" +
            '<span class="confidence-pill">Confidence: ' +
            Math.round((c.confidence || 0) * 100) +
            "%</span>" +
            "</div>" +
            '<div class="meta-row"><span>Component: ns=' +
            escapeHTML(comp.namespace || "-") +
            " workload=" +
            escapeHTML(comp.workload || "-") +
            " pod=" +
            escapeHTML(comp.pod || "-") +
            (comp.node ? " node=" + escapeHTML(comp.node) : "") +
            "</span></div>" +
            '<p class="cause-summary">' +
            escapeHTML(c.summary) +
            "</p>" +
            '<table class="evidence-table">' +
            "<thead><tr><th>Source</th><th>Timestamp (UTC)</th><th>Correlated Evidence</th></tr></thead>" +
            "<tbody>" +
            evRows +
            "</tbody></table>" +
            "</div>"
          );
        })
        .join("");
    }

    detailEl.innerHTML =
      '<div class="detail-header">' +
      "<h2>Incident: " +
      escapeHTML(inc.id) +
      "</h2>" +
      '<div class="subtitle">Window: ' +
      escapeHTML(win.from || "-") +
      " &rarr; " +
      escapeHTML(win.to || "-") +
      "</div>" +
      "</div>" +
      '<div class="meta-grid">' +
      '<div class="meta-box"><div class="label">Status</div><div class="value">' +
      escapeHTML(rep.status || "unknown") +
      "</div></div>" +
      '<div class="meta-box"><div class="label">Source</div><div class="value">' +
      escapeHTML(inc.source || "live") +
      "</div></div>" +
      '<div class="meta-box"><div class="label">Traces / Events / Pods</div><div class="value">' +
      escapeHTML(inc.traceCount || 0) +
      " / " +
      escapeHTML(inc.eventCount || 0) +
      " / " +
      escapeHTML(inc.podCount || 0) +
      "</div></div>" +
      '<div class="meta-box"><div class="label">Target Trace ID</div><div class="value">' +
      escapeHTML(inc.traceId || "(window)") +
      "</div></div>" +
      "</div>" +
      costHTML +
      causesHTML;
  }

  form.addEventListener("submit", async function (ev) {
    ev.preventDefault();
    errEl.classList.add("hidden");
    const payload = {
      traceId: document.getElementById("input-trace-id").value.trim(),
      runDir: document.getElementById("input-run-dir").value.trim(),
      from: document.getElementById("input-from").value.trim(),
      to: document.getElementById("input-to").value.trim(),
    };
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
  });

  filterStatus.addEventListener("change", fetchIncidents);
  filterCause.addEventListener("change", fetchIncidents);
  btnRefresh.addEventListener("click", fetchIncidents);

  fetchIncidents().catch(function (e) {
    listEl.innerHTML =
      '<li class="empty-state">Error loading incidents: ' +
      escapeHTML(e.message) +
      "</li>";
  });
})();
