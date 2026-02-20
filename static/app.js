const form = document.getElementById("search-form");
const patternInput = document.getElementById("pattern");
const groupInput = document.getElementById("group-id");
const tokenInput = document.getElementById("token");
const resultsEl = document.getElementById("results");
const statusEl = document.getElementById("status");
const counterEl = document.getElementById("counter");
const searchBtn = document.getElementById("search-btn");
const stopBtn = document.getElementById("stop-btn");
const tpl = document.getElementById("result-template");

let abortController = null;
let count = 0;

form.addEventListener("submit", (event) => {
  event.preventDefault();
  startSearch().catch((error) => {
    if (error && error.name === "AbortError") {
      setStatus("Search stopped.", false);
      return;
    }
    setStatus(error?.message || "Search failed", true);
  }).finally(() => {
    setControlsIdle();
  });
});

stopBtn.addEventListener("click", () => {
  if (abortController) {
    abortController.abort();
  }
});

async function startSearch() {
  const pattern = patternInput.value.trim();
  const groupId = groupInput.value.trim();
  const token = tokenInput.value.trim();

  if (!pattern) return;

  if (abortController) {
    abortController.abort();
  }

  abortController = new AbortController();
  resultsEl.textContent = "";
  count = 0;
  updateCounter();
  setControlsBusy();
  setStatus(
    groupId
      ? `Searching for "${pattern}" in group ${groupId}...`
      : `Searching for "${pattern}" using server default group...`,
    false
  );

  const response = await fetch("/api/search/stream", {
    method: "POST",
    headers: {
      "Content-Type": "application/json",
    },
    body: JSON.stringify({
      pattern,
      group_id: groupId,
      token,
    }),
    signal: abortController.signal,
  });

  if (!response.ok) {
    const message = await response.text();
    throw new Error(message || `Search failed (${response.status})`);
  }

  if (!response.body) {
    throw new Error("Missing streaming response body");
  }

  const reader = response.body.getReader();
  const decoder = new TextDecoder();
  let buffer = "";

  while (true) {
    const { value, done } = await reader.read();
    if (done) {
      break;
    }

    buffer += decoder.decode(value, { stream: true });
    const lines = buffer.split("\n");
    buffer = lines.pop() || "";

    for (const line of lines) {
      const trimmed = line.trim();
      if (!trimmed) continue;
      handleMessage(trimmed);
    }
  }

  const finalChunk = decoder.decode();
  if (finalChunk) {
    buffer += finalChunk;
  }
  const tail = buffer.trim();
  if (tail) {
    handleMessage(tail);
  }
}

function handleMessage(rawLine) {
  let payload;
  try {
    payload = JSON.parse(rawLine);
  } catch {
    return;
  }

  switch (payload.type) {
    case "status":
      setStatus(payload.message || "Searching...", false);
      break;
    case "result":
      renderResult(payload.result || {});
      count += 1;
      updateCounter();
      break;
    case "done":
      setStatus(`Done. ${payload.total ?? count} result(s).`, false);
      break;
    case "error":
      setStatus(payload.message || "Search failed", true);
      break;
    default:
      break;
  }
}

function renderResult(result) {
  const node = tpl.content.firstElementChild.cloneNode(true);
  node.querySelector(".repo").textContent = result.repo || "unknown repo";
  node.querySelector(".branch").textContent = `branch: ${result.branch || "n/a"}`;

  const link = node.querySelector(".file-link");
  link.href = result.url || "#";
  link.textContent = result.path || result.url || "open file";

  node.querySelector(".snippet").textContent = result.data || "(no snippet)";
  resultsEl.prepend(node);
}

function updateCounter() {
  counterEl.textContent = `${count} result${count > 1 ? "s" : ""}`;
}

function setStatus(message, isError) {
  statusEl.textContent = message;
  statusEl.classList.toggle("status-error", Boolean(isError));
}

function setControlsBusy() {
  searchBtn.disabled = true;
  stopBtn.disabled = false;
}

function setControlsIdle() {
  searchBtn.disabled = false;
  stopBtn.disabled = true;
  abortController = null;
}
