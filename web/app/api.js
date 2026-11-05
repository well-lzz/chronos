let sessionPromise;
let csrf = "";

function fragmentToken() {
  const params = new URLSearchParams(window.location.hash.slice(1));
  const token = params.get("token") || "";
  if (params.has("token")) {
    params.delete("token");
    const fragment = params.toString();
    window.history.replaceState(null, "", window.location.pathname + window.location.search + (fragment ? `#${fragment}` : ""));
  }
  return token;
}

export function consoleSession(token) {
  if (token !== undefined) sessionPromise = undefined;
  if (!sessionPromise) {
    sessionPromise = (async () => {
      const accessToken = token ?? fragmentToken();
      const response = await fetch("/api/session", {
        method: accessToken ? "POST" : "GET",
        credentials: "same-origin",
        headers: accessToken ? { Authorization: `Bearer ${accessToken}`, "Content-Type": "application/json" } : {},
        ...(accessToken ? { body: "{}" } : {}),
      });
      if (!response.ok) throw new Error("请使用终端输出的控制台访问链接，或输入访问令牌。");
      const session = await response.json();
      csrf = session.csrf;
    })().catch((error) => { sessionPromise = undefined; throw error; });
  }
  return sessionPromise;
}

export async function apiFetch(url, options = {}) {
  await consoleSession();
  const headers = new Headers(options.headers);
  if (!["GET", "HEAD", "OPTIONS"].includes((options.method || "GET").toUpperCase())) headers.set("X-Chronos-CSRF", csrf);
  const response = await fetch(url, { ...options, headers, credentials: "same-origin" });
  if (response.status === 401) {
    sessionPromise = undefined;
    window.dispatchEvent(new Event("chronos-auth-required"));
  }
  return response;
}
