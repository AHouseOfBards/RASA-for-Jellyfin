/* RASA wizard client.
 *
 * The server owns the flow; this file owns nothing but rendering. Every screen
 * transition, every branch and every error comes from a Model snapshot pushed
 * over the event stream, so the page can be reloaded at any point in a
 * two-minute install and pick up exactly where it was.
 *
 * No framework, no build step. The whole page is served from loopback under a
 * policy that allows nothing from anywhere else, and a dependency would have to
 * be vendored to load at all.
 */
"use strict";

const TOKEN = document.querySelector('meta[name="rasa-token"]').content;
const DYNU_SIGNUP = "https://www.dynu.com/en-US/ControlPanel/CreateAccount";

/* Transport ---------------------------------------------------------------- */

async function post(path, body) {
  const res = await fetch(path, {
    method: "POST",
    headers: { "Content-Type": "application/json", "X-RASA-Token": TOKEN },
    body: JSON.stringify(body || {}),
  });
  if (!res.ok) {
    status("That didn't go through. Try again.");
  }
  return res;
}

async function get(path) {
  const res = await fetch(path, { headers: { "X-RASA-Token": TOKEN } });
  return res.ok ? res.json() : null;
}

/* Rendering ---------------------------------------------------------------- */

let model = null;
let installStarted = false;
let nameTouched = false;

const GLYPH = {
  pending: "○",  // hollow circle
  running: "●",  // filled circle
  done: "✓",     // check
  failed: "✕",   // cross
  skipped: "–",  // en dash: took a different path, not a failure
};

function status(text) {
  document.getElementById("status").textContent = text || "";
}

function show(screen) {
  for (const el of document.querySelectorAll(".screen")) {
    if (el.dataset.screen === screen) {
      el.setAttribute("data-active", "");
    } else {
      el.removeAttribute("data-active");
    }
  }
}

function renderSteps(id, steps) {
  const list = document.getElementById(id);
  list.replaceChildren();
  for (const s of steps || []) {
    const li = document.createElement("li");
    li.dataset.state = s.state;

    const glyph = document.createElement("span");
    glyph.className = "glyph";
    glyph.textContent = GLYPH[s.state] || GLYPH.pending;
    glyph.setAttribute("aria-hidden", "true");

    const text = document.createElement("span");
    const label = document.createElement("span");
    label.className = "label";
    label.textContent = s.label;
    text.appendChild(label);
    if (s.note) {
      const note = document.createElement("span");
      note.className = "note";
      note.textContent = s.note;
      text.appendChild(note);
    }

    li.append(glyph, text);
    list.appendChild(li);
  }
}

function renderProblem(err) {
  const box = document.getElementById("blocked-problem");
  box.replaceChildren();
  if (!err) return;

  const msg = document.createElement("p");
  msg.className = "message";
  msg.textContent = err.message;
  box.appendChild(msg);

  if (err.why) {
    const why = document.createElement("p");
    why.className = "why";
    why.textContent = err.why;
    box.appendChild(why);
  }
  if (err.partial) {
    const partial = document.createElement("p");
    partial.className = "partial";
    partial.textContent = err.partial;
    box.appendChild(partial);
  }

  const actions = document.getElementById("blocked-actions");
  actions.replaceChildren();
  for (const a of err.actions || []) {
    const b = document.createElement("button");
    b.className = a.kind === "retry" ? "primary" : "secondary";
    b.textContent = a.label;
    b.addEventListener("click", () => handleAction(a));
    actions.appendChild(b);
  }
}

/* An action's meaning depends on where setup stopped, which the model already
 * knows. Retry re-runs the step that failed rather than starting over, because
 * starting over is exactly what a user with a slow DNS provider does not need. */
function handleAction(action) {
  switch (model && model.screen) {
    case "blocked":
      post("/api/start");
      break;
    case "name":
      show("name");
      break;
    case "port":
      post("/api/port/open");
      break;
    default:
      post("/api/install");
  }
}

function renderName() {
  const select = document.getElementById("name-parent");
  if (select.options.length === 0) {
    for (const d of model.domains || []) {
      const opt = document.createElement("option");
      opt.value = d.name;
      opt.textContent = "." + d.name;
      opt.selected = !!d.default;
      select.appendChild(opt);
    }
  }
  const label = document.getElementById("name-label");
  if (!nameTouched && model.name && model.name.label) {
    label.value = model.name.label;
  }
  updatePreview();
}

function updatePreview() {
  const label = document.getElementById("name-label").value.trim();
  const parent = document.getElementById("name-parent").value;
  const preview = document.getElementById("name-preview");
  preview.textContent = label ? `https://${label}.${parent}` : "";
  document.getElementById("name-submit").disabled = label.length === 0;
}

function renderPort() {
  const p = model.port || {};
  const guide = document.getElementById("port-guide");
  const lede = document.getElementById("port-lede");

  if (!p.instructions || p.instructions.length === 0) {
    guide.hidden = true;
    // Only claim to be working when something actually is. This screen used to
    // say "checking..." while sitting idle waiting for a click.
    if (model.busy) {
      lede.textContent = "Asking your router to open the port.";
    } else if (p.open) {
      lede.textContent = "Your router opened the port on its own. Nothing for you to do.";
    } else {
      lede.textContent = "Your router didn't open the port, and RASA couldn't work out how to guide you through it. You can try again, or continue without it.";
    }
    return;
  }

  guide.hidden = false;
  document.getElementById("port-title").textContent = "One thing to do on your router";
  lede.textContent = p.open && !p.permanent
    ? "Your router opened the port, but it will forget when it restarts. Making it permanent takes a minute."
    : "Your router won't open the port on its own, so it needs one rule adding. Everything you need is below.";

  document.getElementById("port-router").textContent = p.router_name || "Your router";

  const steps = document.getElementById("port-steps");
  steps.replaceChildren();
  for (const s of p.instructions) {
    const li = document.createElement("li");
    li.textContent = s.text;
    steps.appendChild(li);
  }

  const values = document.getElementById("port-values");
  values.replaceChildren();
  for (const v of p.values || []) {
    const tr = document.createElement("tr");
    const th = document.createElement("th");
    th.scope = "row";
    th.textContent = v.label;
    const td = document.createElement("td");
    td.textContent = v.value;
    tr.append(th, td);
    values.appendChild(tr);
  }

  const reservation = document.getElementById("port-reservation");
  reservation.hidden = !p.reservation_note;
  reservation.textContent = p.reservation_note || "";
}

function renderDone() {
  document.getElementById("final-url").textContent = model.result.url || "";

  // The QR is inlined as a data URI: the page loads under a policy that
  // permits nothing from anywhere else, and the address should not need a
  // second request to appear.
  const figure = document.getElementById("qr-figure");
  const hint = document.getElementById("qr-hint");
  if (model.result.qr_png) {
    document.getElementById("qr-image").src = model.result.qr_png;
    figure.hidden = false;
    hint.hidden = false;
  } else {
    figure.hidden = true;
    hint.hidden = true;
  }

  document.getElementById("recovery-path").textContent = model.result.recovery_file || "";
  document.getElementById("reach-note").textContent =
    model.result.reachable === "reachable" ? "" : (model.result.reach_message || "");

  const box = document.getElementById("warnings");
  const list = document.getElementById("warning-list");
  list.replaceChildren();
  for (const w of model.warnings || []) {
    const li = document.createElement("li");
    li.textContent = w.text;
    list.appendChild(li);
  }
  box.hidden = list.childElementCount === 0;
}

function renderRemoved() {
  document.getElementById("removed-detail").textContent =
    "Nothing is listening for connections from outside any more, and the stored key has been deleted.";

  const list = document.getElementById("removed-warning-list");
  list.replaceChildren();
  for (const w of model.warnings || []) {
    const li = document.createElement("li");
    li.textContent = w.text;
    list.appendChild(li);
  }
  document.getElementById("removed-warnings").hidden = list.childElementCount === 0;
}

function render(next) {
  model = next;

  document.getElementById("version").textContent = model.version || "";
  renderSteps("checks", model.checks);
  renderSteps("setup", model.setup);
  renderSteps("removal", model.removal);

  for (const b of document.querySelectorAll("button")) {
    if (b.dataset.action !== "copy" && b.dataset.action !== "open") {
      b.disabled = model.busy;
    }
  }
  document.getElementById("name-submit").disabled =
    model.busy || document.getElementById("name-label").value.trim().length === 0;

  document.getElementById("remove-button").hidden = !model.repair;
  if (model.repair) {
    const notice = document.getElementById("repair-notice");
    notice.hidden = false;
    document.getElementById("repair-detail").textContent =
      model.name && model.name.hostname ? `Your address is ${model.name.hostname}.` : "";
    document.getElementById("welcome-title").textContent = "Check your remote access";
    document.getElementById("welcome-button").textContent = "Check and repair";
  }

  if (model.jellyfin) {
    const found = document.getElementById("jellyfin-found");
    if (model.jellyfin.server_name) {
      found.textContent = `Found "${model.jellyfin.server_name}" running ${model.jellyfin.version} at ${model.jellyfin.address}.`;
    } else if (model.jellyfin.address) {
      found.textContent = `Found Jellyfin at ${model.jellyfin.address}.`;
    }
  }

  if (model.screen === "name") renderName();
  if (model.screen === "port") renderPort();
  if (model.screen === "done") renderDone();
  if (model.screen === "removed") renderRemoved();

  renderProblem(model.err);
  if (model.err && model.screen !== "blocked") {
    status(model.err.message);
  } else if (!model.busy) {
    status("");
  }

  show(model.screen);

  // The setup screen is reached without a click: the port step advances to it
  // on its own once the path is open. Starting the pipeline here is what makes
  // that seamless, and the guard is what stops a reconnecting page from
  // starting it a second time.
  if (model.screen === "setup" && !model.busy && !installStarted && !model.err) {
    installStarted = true;
    post("/api/install");
  }
  if (model.screen !== "setup") {
    installStarted = model.screen === "done";
  }
}

/* Name field --------------------------------------------------------------- */

let checkTimer = null;

function scheduleCheck() {
  nameTouched = true;
  updatePreview();
  clearTimeout(checkTimer);

  const advice = document.getElementById("name-advice");
  advice.textContent = "";
  advice.removeAttribute("data-tone");
  document.getElementById("name-suggestions").hidden = true;

  // Debounced, because this is a DNS lookup per keystroke otherwise. The
  // answer is advisory in any case: a name that does not resolve is not proof
  // it is free, so nothing here ever blocks the user from trying.
  checkTimer = setTimeout(runCheck, 400);
}

async function runCheck() {
  const label = document.getElementById("name-label").value.trim();
  const parent = document.getElementById("name-parent").value;
  if (!label) return;

  const view = await get(`/api/name/check?label=${encodeURIComponent(label)}&parent=${encodeURIComponent(parent)}`);
  if (!view) return;
  if (document.getElementById("name-label").value.trim() !== label) return; // stale

  const advice = document.getElementById("name-advice");
  advice.textContent = view.advice || "";
  if (view.availability === "unclaimed" || view.availability === "mine") {
    advice.dataset.tone = "good";
  } else if (view.availability === "in_use") {
    advice.dataset.tone = "bad";
  } else {
    advice.removeAttribute("data-tone");
  }

  const box = document.getElementById("name-suggestions");
  const chips = document.getElementById("name-chips");
  chips.replaceChildren();
  for (const s of view.suggestions || []) {
    const b = document.createElement("button");
    b.type = "button";
    b.textContent = s;
    b.addEventListener("click", () => {
      const dot = s.indexOf(".");
      document.getElementById("name-label").value = s.slice(0, dot);
      document.getElementById("name-parent").value = s.slice(dot + 1);
      box.hidden = true;
      scheduleCheck();
    });
    chips.appendChild(b);
  }
  box.hidden = chips.childElementCount === 0;
}

/* Wiring ------------------------------------------------------------------- */

function wire() {
  document.getElementById("name-label").addEventListener("input", scheduleCheck);
  document.getElementById("name-parent").addEventListener("change", scheduleCheck);

  document.getElementById("signin-form").addEventListener("submit", (e) => {
    e.preventDefault();
    post("/api/jellyfin/signin", {
      username: document.getElementById("jf-user").value,
      password: document.getElementById("jf-pass").value,
    });
    document.getElementById("jf-pass").value = "";
  });

  document.getElementById("apikey-form").addEventListener("submit", (e) => {
    e.preventDefault();
    post("/api/jellyfin/signin", { api_key: document.getElementById("jf-key").value });
    document.getElementById("jf-key").value = "";
  });

  document.getElementById("dynu-form").addEventListener("submit", (e) => {
    e.preventDefault();
    post("/api/dynu/key", { key: document.getElementById("dynu-key").value });
    document.getElementById("dynu-key").value = "";
  });

  document.getElementById("name-form").addEventListener("submit", (e) => {
    e.preventDefault();
    post("/api/name", {
      label: document.getElementById("name-label").value.trim(),
      parent: document.getElementById("name-parent").value,
    });
  });

  for (const b of document.querySelectorAll("[data-toggle]")) {
    b.addEventListener("click", () => {
      const wantKey = b.dataset.toggle === "apikey";
      document.getElementById("signin-form").hidden = wantKey;
      document.getElementById("apikey-form").hidden = !wantKey;
    });
  }

  document.addEventListener("click", async (e) => {
    const action = e.target.dataset && e.target.dataset.action;
    if (!action) return;

    switch (action) {
      case "start":
        post("/api/start");
        break;
      case "remove":
        // The one destructive thing in the product, so it asks. Uninstalling
        // RASA deliberately leaves remote access running; this is the button
        // that does not.
        if (confirm("Remove remote access?\n\nYour server will stop being reachable from outside your home network. Jellyfin itself, and your logs, are left alone.")) {
          post("/api/remove");
        }
        break;
      case "port-open":
        post("/api/port/open");
        break;
      case "port-skip":
        post("/api/port/skip");
        break;
      case "quit":
        await post("/api/quit");
        status("You can close this window.");
        break;
      case "open-dynu":
        // A new tab rather than an embedded frame: Dynu, like every sign-up
        // page worth trusting, refuses to be framed. The paste field stays
        // here so the user comes straight back.
        window.open(DYNU_SIGNUP, "_blank", "noopener");
        break;
      case "paste":
        try {
          document.getElementById("dynu-key").value = await navigator.clipboard.readText();
          status("Pasted.");
        } catch {
          // Clipboard access needs permission the browser may refuse. The
          // field is right there, so this is a convenience, never the only way.
          status("Your browser wouldn't share the clipboard. Paste into the box instead.");
        }
        break;
      case "copy":
        try {
          await navigator.clipboard.writeText(model.result.url);
          status("Address copied.");
        } catch {
          status("Select the address and copy it.");
        }
        break;
      case "open":
        window.open(model.result.url, "_blank", "noopener");
        break;
    }
  });
}

/* Event stream ------------------------------------------------------------- */

function connect() {
  const source = new EventSource(`/api/events?t=${encodeURIComponent(TOKEN)}`);
  source.onmessage = (e) => render(JSON.parse(e.data));
  source.onerror = () => {
    // EventSource reconnects on its own. Setup keeps running regardless: the
    // wizard is not driven by this connection, only reported through it.
    status("Reconnecting…");
  };
}

wire();
get("/api/state").then((m) => { if (m) render(m); });
connect();
