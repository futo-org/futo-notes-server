// Mounts the web provider's embedded checkout (Stripe) in every
// [data-checkout] panel, on page load and whenever htmx swaps one in.
// The panel's session URL opens the checkout; once it completes, the
// return URL the server answered with confirms it and shows the result.
(() => {
  "use strict";
  let current = null;

  async function openSession(panel) {
    const headers = { Accept: "application/json" };
    const csrf = document.querySelector('meta[name="csrf-token"]');
    if (csrf) headers["X-CSRF-Token"] = csrf.content;
    const res = await fetch(panel.dataset.sessionUrl, { method: "POST", headers, credentials: "same-origin" });
    const body = await res.json().catch(() => ({}));
    if (!res.ok) throw new Error(body.message || body.error || "The checkout could not be opened. Try again.");
    return body;
  }

  function finish(panel, returnURL) {
    // Inside the dashboard, swap the result in place; elsewhere, load it.
    if (window.htmx && panel.closest("#page") && document.querySelector('meta[name="csrf-token"]')) {
      window.htmx.ajax("GET", returnURL, { target: "#page", swap: "innerHTML" });
    } else {
      window.location.assign(returnURL);
    }
  }

  async function mount(panel) {
    if (panel.dataset.mounted) return;
    panel.dataset.mounted = "true";
    const status = panel.querySelector("[data-checkout-status]");
    // A panel swapped into a page served without the checkout's policy
    // cannot load Stripe.js; load a page that hosts it instead.
    if (!document.querySelector('meta[name="checkout-host"]') || typeof window.Stripe !== "function") {
      window.location.assign(panel.dataset.pageUrl);
      return;
    }
    let returnURL = "";
    try {
      if (current) {
        current.destroy();
        current = null;
      }
      const stripe = window.Stripe(panel.dataset.publishableKey);
      current = await stripe.createEmbeddedCheckoutPage({
        fetchClientSecret: async () => {
          const session = await openSession(panel);
          returnURL = session.return_url;
          return session.client_secret;
        },
        onComplete: () => {
          status.textContent = "Confirming your subscription…";
          if (current) {
            current.destroy();
            current = null;
          }
          finish(panel, returnURL);
        },
      });
      current.mount(panel.querySelector("[data-checkout-mount]"));
      status.textContent = "";
    } catch (err) {
      status.textContent = err.message;
    }
  }

  function scan(root) {
    if (root.matches && root.matches("[data-checkout]")) mount(root);
    if (root.querySelectorAll) root.querySelectorAll("[data-checkout]").forEach(mount);
  }

  document.addEventListener("DOMContentLoaded", () => scan(document));
  document.addEventListener("htmx:load", (event) => scan(event.detail.elt));
})();
