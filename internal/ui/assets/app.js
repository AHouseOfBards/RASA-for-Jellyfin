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
// Straight to the key, rather than leaving the user to find "Control Panel →
// API Credentials" themselves. Signed out this redirects to the login page
// carrying a ReturnUrl back here, so signing in lands on the right page.
const DYNU_API_KEYS = "https://www.dynu.com/en-US/ControlPanel/APICredentials";
// Where a full account is emptied. Offered when Dynu refuses a new address
// because the free allowance is used up.
const DYNU_DDNS = "https://www.dynu.com/en-US/ControlPanel/DDNS";

/* The six steps, in order.
 *
 * The welcome screen promises six steps and three at the keyboard, so the
 * count has to come from one place or the promise quietly stops being true.
 * Screens outside the run - welcome, blocked, done, removing, removed - have
 * no position in it and hide the rail.
 *
 * One capitalised word each, so six of them read as a set at a glance rather
 * than as a sentence someone got bored halfway through. They also have to mean
 * something to a reader who has not reached them yet, which rules out the
 * metaphor the port screen uses in its own heading: "the door" lands once you
 * are on it and says nothing three steps early. And naming the address step
 * after the user was simply wrong - it is the server's address being chosen,
 * not their name. */
const JOURNEY = [
  { screen: "checking", label: "Checks" },
  { screen: "jellyfin", label: "Jellyfin" },
  { screen: "dynu", label: "Account" },
  { screen: "name", label: "Address" },
  { screen: "port", label: "Router" },
  { screen: "setup", label: "Setup" },
];

/* Which screens are waiting for a person, and what the tab should say.
 *
 * The tab title is the notification here. There is a wait in the middle of
 * this run that has been measured at anywhere from two seconds to five
 * minutes, and users walk away during it; something has to call them back that
 * works without a permission prompt. */
const TAB_TITLE = {
  welcome: { text: "Set up remote access", waiting: false },
  checking: { text: "Checking your network", waiting: false },
  blocked: { text: "Needs your attention", waiting: true },
  jellyfin: { text: "Sign in to Jellyfin", waiting: true },
  dynu: { text: "Your Dynu account", waiting: true },
  name: { text: "Choose your address", waiting: true },
  port: { text: "Your router", waiting: true },
  setup: { text: "Setting everything up", waiting: false },
  done: { text: "Setup finished", waiting: true },
  removing: { text: "Removing remote access", waiting: false },
  removed: { text: "Remote access removed", waiting: true },
};

/* The field to put the cursor in when a screen appears.
 *
 * The plain autofocus attribute only fires on page load, and every screen here
 * is shown by script, so without this every form screen needed a mouse click
 * before the user could type a single character. */
const FOCUS_TARGET = {
  jellyfin: "jf-user",
  dynu: "dynu-key",
  name: "name-label",
  welcome: "welcome-button",
};

/* Transport ---------------------------------------------------------------- */

async function post(path, body) {
  const res = await fetch(path, {
    method: "POST",
    headers: { "Content-Type": "application/json", "X-RASA-Token": TOKEN },
    body: JSON.stringify(body || {}),
  });
  if (!res.ok) {
    // Naming the cause matters: "that didn't go through" reads the same for a
    // wizard that has already exited as it does for a transient blip, and only
    // one of those is worth clicking again.
    status(res.status === 403
      ? "This page's key is no longer valid. Close it and start RASA again."
      : `That didn't go through (error ${res.status}). Try again.`);
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
// Set once the wizard has been told to quit. The server stops serving as a
// result, so every snapshot, reconnect and status line after that point is
// noise about a teardown the user deliberately asked for.
let closing = false;
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

let shownScreen = null;

function show(screen) {
  for (const el of document.querySelectorAll(".screen")) {
    if (el.dataset.screen === screen) {
      el.setAttribute("data-active", "");
    } else {
      el.removeAttribute("data-active");
    }
  }
  renderRail(screen);
  // Only on an actual change. Re-running this on every model snapshot would
  // yank the cursor out of the field mid-word, and snapshots arrive often.
  if (screen !== shownScreen) {
    shownScreen = screen;
    focusScreen(screen);
  }
  renderTitle(screen);
}

/* Moves the cursor, and the screen reader, to the new screen.
 *
 * Swapping screens without moving focus leaves a keyboard or screen-reader
 * user parked on the button they just pressed, on a screen that is no longer
 * visible, with no announcement that anything happened. */
function focusScreen(screen) {
  const target = FOCUS_TARGET[screen];
  const el = target && document.getElementById(target);
  if (el && !el.disabled && el.offsetParent !== null) {
    el.focus();
    return;
  }
  const section = document.querySelector(`.screen[data-screen="${screen}"]`);
  const heading = section && section.querySelector("h1");
  if (heading) {
    heading.setAttribute("tabindex", "-1");
    heading.focus();
  }
}

/* The tab title, which is this product's only notification channel. */
function renderTitle(screen) {
  const entry = TAB_TITLE[screen] || { text: "Set up remote access", waiting: false };
  const step = JOURNEY.findIndex((s) => s.screen === screen);
  const where = step >= 0 ? ` (${step + 1}/${JOURNEY.length})` : "";
  // The marker goes first so it survives the truncation a narrow tab applies,
  // and only appears when the user is genuinely being waited on - a marker on
  // every screen is not a signal.
  const mark = entry.waiting && document.hidden ? "● " : "";
  document.title = `${mark}${entry.text}${where} — RASA`;
}

function renderRail(screen) {
  const nav = document.getElementById("progress");
  const at = JOURNEY.findIndex((s) => s.screen === screen);
  if (at < 0) {
    nav.hidden = true;
    return;
  }
  nav.hidden = false;

  const rail = document.getElementById("rail");
  rail.replaceChildren();
  JOURNEY.forEach((s, i) => {
    const li = document.createElement("li");
    li.dataset.state = i < at ? "done" : i === at ? "here" : "todo";
    li.textContent = s.label;
    if (i === at) li.setAttribute("aria-current", "step");
    rail.appendChild(li);
  });

  document.getElementById("rail-caption").textContent =
    `Step ${at + 1} of ${JOURNEY.length}: ${JOURNEY[at].label}`;
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

/* Screens that host a problem box of their own rather than sending the user to
 * the blocked screen. The name picker does, because its failures are answered
 * by editing the field that is already on screen.
 *
 * Each entry needs "<name>-problem" and "<name>-problem-actions" in the page.
 * One list rather than two, so a screen cannot be cleared without being
 * renderable or the other way round. */
const PROBLEM_SCREENS = ["blocked", "name"];

function problemBox() {
  const screen = (model && model.screen) || "";
  const name = PROBLEM_SCREENS.includes(screen) ? screen : "blocked";
  return [
    document.getElementById(name + "-problem"),
    document.getElementById(name + "-problem-actions"),
  ];
}

function renderProblem(err) {
  // Every box is cleared, not just this screen's, so an error never lingers on
  // a screen it did not belong to.
  for (const name of PROBLEM_SCREENS) {
    document.getElementById(name + "-problem").replaceChildren();
    document.getElementById(name + "-problem-actions").replaceChildren();
  }
  const [box, actions] = problemBox();
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
 * starting over is exactly what a user with a slow DNS provider does not need.
 *
 * A few actions mean the same thing wherever they appear, and those are matched
 * by id first — sending someone who asked to open Dynu back to the name box
 * instead is worse than not offering the button. */
function handleAction(action) {
  if (action.id === "open_dynu") {
    window.open(DYNU_DDNS, "_blank", "noopener");
    return;
  }
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

function closeConfirm() {
  document.getElementById("name-confirm").hidden = true;
  document.getElementById("name-actions").hidden = false;
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

  document.getElementById("port-upnp").hidden = !p.automatic_off;

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

  const guess = document.getElementById("port-guess");
  guess.hidden = !p.router_guessed;
  if (p.router_guessed) {
    document.getElementById("port-guess-name").textContent = p.router_name || "";
  }

  const note = document.getElementById("port-note");
  note.textContent = p.router_note || "";
  note.hidden = !p.router_note;

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

// The headline states only what was actually established.
//
// It used to read "Your server is reachable" unconditionally, directly above a
// note explaining that reachability could not be confirmed - and on a network
// where inbound connections are firewalled outright, that headline is simply
// false. Three outcomes, three headlines.
const DONE_COPY = {
  reachable: {
    title: "Your server is reachable",
    lede: "Use this address from anywhere. It works in a browser and in every Jellyfin app.",
  },
  inconclusive: {
    title: "Setup is finished",
    lede: "Everything is configured. Whether it can be reached from outside couldn't be checked from this network. See below.",
  },
  unreachable: {
    title: "Setup is finished, but nothing can reach it yet",
    lede: "Your address and certificate are ready. The last step is letting connections in from outside. See below.",
  },
};

function renderDone() {
  const copy = DONE_COPY[model.result.reachable] || DONE_COPY.inconclusive;
  document.getElementById("done-title").textContent = copy.title;
  document.getElementById("done-lede").textContent = copy.lede;

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

  // Only once the address is on screen, so the user can test before removing.
  const offer = document.getElementById("uninstall-offer");
  const offerHint = document.getElementById("uninstall-hint");
  const offerButton = document.getElementById("uninstall-button");
  offer.hidden = !(model.result.can_uninstall || model.result.uninstall_hint);
  offerButton.hidden = !model.result.can_uninstall;
  offerHint.hidden = !model.result.uninstall_hint;
  offerHint.textContent = model.result.uninstall_hint
    ? "On this system there is nothing to launch. Run: " + model.result.uninstall_hint
    : "";

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

/* The end of the run.
 *
 * Set before the quit request rather than after it: the server stops serving
 * as a result of that request, so anything waiting on its response is waiting
 * on a connection that is being torn down. */
function farewell() {
  closing = true;

  const removed = model && model.screen === "removed";
  document.getElementById("closed-title").textContent =
    removed ? "You can close this tab" : "All set, you can close this tab";
  document.getElementById("closed-lede").textContent = removed
    ? "Remote access has been removed and RASA has stopped running."
    : "Setup has finished and RASA has stopped running. Your remote access keeps working without it.";

  const url = model && model.result && model.result.url;
  const box = document.getElementById("closed-address");
  if (url && !removed) {
    document.getElementById("closed-url").textContent = url;
    box.hidden = false;
  } else {
    box.hidden = true;
  }

  const recovery = model && model.result && model.result.recovery_file;
  document.getElementById("closed-recovery").textContent =
    recovery && !removed ? `Everything is written down in ${recovery}.` : "";

  status("");
  show("closed");
  // After show, which sets a title of its own from the screen name.
  document.title = removed ? "Removed — RASA" : "All set — RASA";

  // Refused for a tab the operating system opened, which is this one, but it
  // costs nothing and does work when a browser was configured to allow it.
  try { window.close(); } catch { /* expected */ }
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
  if (closing) return;
  model = next;

  document.getElementById("version").textContent = model.version || "";
  renderSteps("checks", model.checks);
  renderSteps("setup", model.setup);
  renderSteps("removal", model.removal);

  for (const b of document.querySelectorAll("button")) {
    const alwaysLive =
      b.dataset.action === "copy" ||
      b.dataset.action === "open" ||
      b.dataset.keepEnabled !== undefined;
    if (!alwaysLive) b.disabled = model.busy;
  }
  document.getElementById("name-submit").disabled =
    model.busy || document.getElementById("name-label").value.trim().length === 0;

  // The server decides where back can go, because it is the only thing that
  // knows what has already been created for real.
  document.getElementById("back-button").hidden = !model.can_back;

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

/* Dynu key field ----------------------------------------------------------- */

let keyTimer = null;

function scheduleKeyCheck() {
  clearTimeout(keyTimer);
  const advice = document.getElementById("dynu-advice");
  advice.textContent = "";
  advice.removeAttribute("data-tone");
  // Longer than the name field's debounce, because this one costs a round trip
  // to Dynu rather than a DNS lookup, and the realistic gesture here is a
  // paste that arrives complete in one event.
  keyTimer = setTimeout(runKeyCheck, 500);
}

async function runKeyCheck() {
  const key = document.getElementById("dynu-key").value.trim();
  if (!key) return;

  const res = await fetch("/api/dynu/check", {
    method: "POST",
    headers: { "Content-Type": "application/json", "X-RASA-Token": TOKEN },
    body: JSON.stringify({ key }),
  });
  if (!res.ok) return;
  const view = await res.json();
  // Stale: the user kept typing while this was in flight.
  if (document.getElementById("dynu-key").value.trim() !== key) return;

  const advice = document.getElementById("dynu-advice");
  advice.textContent = view.message || "";
  if (view.state === "valid") {
    advice.dataset.tone = "good";
  } else if (view.state === "rejected") {
    advice.dataset.tone = "bad";
  } else {
    advice.removeAttribute("data-tone");
  }
}

/* Name field --------------------------------------------------------------- */

let checkTimer = null;

function scheduleCheck() {
  nameTouched = true;
  // Any edit invalidates an address the user was being asked to confirm.
  closeConfirm();
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
  document.getElementById("dynu-key").addEventListener("input", scheduleKeyCheck);
  document.getElementById("back-button").addEventListener("click", () => {
    closeConfirm();
    post("/api/back");
  });

  // The marker only means anything while the tab is in the background, so it
  // has to be recomputed when that changes rather than only on a transition.
  document.addEventListener("visibilitychange", () => {
    if (shownScreen) renderTitle(shownScreen);
  });

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

  // Submitting shows the address for approval; it does not create it. The
  // second click is the one that reaches the server.
  document.getElementById("name-form").addEventListener("submit", (e) => {
    e.preventDefault();
    const label = document.getElementById("name-label").value.trim();
    if (!label) return;
    const parent = document.getElementById("name-parent").value;
    document.getElementById("confirm-url").textContent = `https://${label}.${parent}`;
    document.getElementById("name-confirm").hidden = false;
    document.getElementById("name-actions").hidden = true;
    document.getElementById("confirm-create").focus();
  });

  document.getElementById("confirm-back").addEventListener("click", () => {
    closeConfirm();
    document.getElementById("name-label").focus();
    document.getElementById("name-label").select();
  });

  document.getElementById("confirm-create").addEventListener("click", () => {
    closeConfirm();
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
      case "port-generic":
        post("/api/port/generic");
        break;
      case "port-skip":
        post("/api/port/skip");
        break;
      case "uninstall":
        // Confirmed, because it is the one button here that removes something,
        // and it sits next to a button that removes something else entirely.
        if (!confirm("Remove the RASA setup app?\n\nYour remote access keeps working. This only deletes the setup app itself.")) break;
        farewell();
        await post("/api/uninstall");
        break;
      case "quit":
        farewell();
        await post("/api/quit");
        break;
      case "open-dynu":
        // A new tab rather than an embedded frame: Dynu, like every sign-up
        // page worth trusting, refuses to be framed. The paste field stays
        // here so the user comes straight back.
        window.open(DYNU_SIGNUP, "_blank", "noopener");
        break;
      case "open-dynu-key":
        window.open(DYNU_API_KEYS, "_blank", "noopener");
        break;
      case "paste":
        try {
          document.getElementById("dynu-key").value = await navigator.clipboard.readText();
          status("Pasted.");
          scheduleKeyCheck();
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
    // Expected: quitting is what closed the stream.
    if (closing) { source.close(); return; }
    // EventSource reconnects on its own. Setup keeps running regardless: the
    // wizard is not driven by this connection, only reported through it.
    status("Reconnecting…");
  };
}

wire();
get("/api/state").then((m) => { if (m) render(m); });
connect();
