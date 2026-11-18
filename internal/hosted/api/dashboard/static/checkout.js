// Drives the billing country step ([data-country-step]): as the country
// changes, it shows what the chosen country's route means (tax added on
// Stripe's page, or no web payment at all) and hides Checkout where it
// cannot be used. The server decides the route again when it is posted.
//
// Mounts our checkout in every [data-checkout] panel, on page load and
// whenever htmx swaps one in. The panel's session URL opens a Stripe
// Checkout Session (ui_mode=elements); Stripe's Payment Element collects the
// card inside its own frames, styled with our theme, and our button confirms
// the session. Once it is confirmed, the return URL the server answered with
// confirms it on our side and shows the result.
//
// The card form's own Country field starts at the billing country chosen
// before checkout (data-country), and the button is held, with a message,
// while the field names another. The server refuses such a payment anyway.
(() => {
  "use strict";
  // The Payment Element in the page, destroyed when another panel mounts.
  let current = null;

  const latin = "U+0000-00FF, U+0131, U+0152-0153, U+02BB-02BC, U+02C6, U+02DA, U+02DC, U+0304, U+0308, U+0329, " +
    "U+2000-206F, U+20AC, U+2122, U+2191, U+2193, U+2212, U+2215, U+FEFF, U+FFFD";
  const latinExt = "U+0100-02BA, U+02BD-02C5, U+02C7-02CC, U+02CE-02D7, U+02DD-02FF, U+1D00-1DBF, U+1E00-1E9F, " +
    "U+1EF2-1EFF, U+2020, U+20A0-20AB, U+20AD-20C0, U+2113, U+2C60-2C7F, U+A720-A7FF";

  // Our self-hosted Barlow (the weights the card fields use), for Stripe's
  // frames. Stripe takes only https: or data: font sources, so the page
  // reads the files from its own origin and hands them over as data: URLs,
  // which also needs no cross-origin access to our files.
  async function fonts() {
    const faces = [];
    for (const weight of ["400", "500"]) {
      for (const [subset, unicodeRange] of [["latin", latin], ["latin-ext", latinExt]]) {
        faces.push((async () => {
          const res = await fetch(`/dashboard/static/Barlow-${weight}-${subset}.woff2`);
          if (!res.ok) throw new Error(`font ${res.status}`);
          const blob = await res.blob();
          const src = await new Promise((resolve, reject) => {
            const reader = new FileReader();
            reader.onload = () => resolve(reader.result);
            reader.onerror = () => reject(reader.error);
            reader.readAsDataURL(blob);
          });
          return { family: "Barlow", src: `url(${src})`, weight, unicodeRange, display: "swap" };
        })());
      }
    }
    return Promise.all(faces);
  }

  // The Appearance API object for the current colour scheme, read from the
  // stylesheet's tokens so the card fields match the page.
  function appearance() {
    const css = getComputedStyle(document.documentElement);
    const token = (name) => css.getPropertyValue(name).trim();
    const muted = token("--color-muted");
    const primary = token("--color-primary");
    const danger = token("--color-danger");
    const dark = window.matchMedia("(prefers-color-scheme: dark)").matches;
    return {
      theme: dark ? "night" : "stripe",
      labels: "above",
      variables: {
        colorPrimary: primary,
        colorBackground: token("--color-bg"),
        colorText: token("--color-text"),
        colorTextSecondary: muted,
        colorTextPlaceholder: muted,
        colorDanger: danger,
        iconColor: muted,
        fontFamily: "Barlow, system-ui, -apple-system, sans-serif",
        fontSizeBase: "15px",
        fontWeightNormal: "400",
        fontWeightMedium: "500",
        fontWeightBold: "600",
        spacingUnit: "4px",
        borderRadius: "10px",
        logoColor: dark ? "dark" : "light",
      },
      rules: {
        ".Label": { color: muted, fontSize: "13px", fontWeight: "400", marginBottom: "6px" },
        // A field needs 3:1 against the page to be seen, as our inputs do.
        ".Input": { border: `1px solid ${muted}`, boxShadow: "none", padding: "10px 12px", fontSize: "16px" },
        ".Input:focus": { borderColor: primary, boxShadow: `0 0 0 1px ${primary}`, outline: "none" },
        ".Input--invalid": { borderColor: danger, boxShadow: `0 0 0 1px ${danger}` },
        ".Error": { color: danger, fontSize: "13px", marginTop: "6px" },
      },
    };
  }

  async function openSession(panel) {
    const headers = { Accept: "application/json" };
    const csrf = document.querySelector('meta[name="csrf-token"]');
    if (csrf) headers["X-CSRF-Token"] = csrf.content;
    let res;
    try {
      res = await fetch(panel.dataset.sessionUrl, { method: "POST", headers, credentials: "same-origin" });
    } catch {
      throw new Error("We couldn't reach FUTO Notes. Check your connection and try again.");
    }
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
    // A panel swapped into a page served without the checkout's policy
    // cannot load Stripe.js; load a page that hosts it instead.
    if (!document.querySelector('meta[name="checkout-host"]') || typeof window.Stripe !== "function") {
      window.location.assign(panel.dataset.pageUrl);
      return;
    }
    const form = panel.querySelector("[data-checkout-form]");
    const status = panel.querySelector("[data-checkout-status]");
    const errorBox = panel.querySelector("[data-checkout-error]");
    const submit = panel.querySelector("[data-checkout-submit]");
    const countryBox = panel.querySelector("[data-checkout-country]");
    const chosen = panel.dataset.country;
    const label = panel.querySelector("[data-checkout-label]");
    const idleLabel = label.innerHTML;

    let actions = null;
    let returnURL = "";
    // The card fields are complete, and Stripe can confirm the session.
    let complete = false;
    let canConfirm = false;
    let busy = false;
    // The card form's Country field, as its change events report it.
    let formCountry = chosen;

    const showError = (message) => {
      errorBox.textContent = message;
      errorBox.hidden = !message;
    };
    const update = () => {
      const otherCountry = formCountry !== chosen;
      countryBox.hidden = !otherCountry;
      submit.disabled = busy || !actions || !complete || !canConfirm || otherCountry;
      submit.setAttribute("aria-busy", String(busy));
    };
    const setBusy = (on, text) => {
      busy = on;
      if (on) label.textContent = text;
      else label.innerHTML = idleLabel;
      if (!on) canConfirm = actions.getSession().canConfirm;
      panel.toggleAttribute("data-busy", on);
      update();
    };
    // The amount and period Stripe will charge, as it formats them;
    // Stripe requires the page to show its total before confirming.
    const show = (session) => {
      const amount = session.total.total.amount;
      const recurring = session.recurring;
      const period = recurring && recurring.intervalCount > 1
        ? `${recurring.intervalCount} ${recurring.interval}s`
        : recurring ? recurring.interval : "";
      panel.querySelectorAll("[data-checkout-amount]").forEach((el) => { el.textContent = amount; });
      if (period) panel.querySelectorAll("[data-checkout-period]").forEach((el) => { el.textContent = period; });
      canConfirm = session.canConfirm;
    };

    if (current) {
      current.destroy();
      current = null;
    }
    try {
      const stripe = window.Stripe(panel.dataset.publishableKey);
      const checkout = stripe.initCheckoutElementsSdk({
        clientSecret: openSession(panel).then((session) => {
          returnURL = session.return_url;
          return session.client_secret;
        }),
        elementsOptions: { appearance: appearance(), loader: "auto" },
        adaptivePricing: { allowed: false },
        defaultValues: { billingAddress: { address: { country: chosen } } },
      });
      // Without Barlow the fields keep the fallback fonts; nothing else waits.
      fonts().then((faces) => checkout.loadFonts(faces)).catch(() => {});
      const scheme = window.matchMedia("(prefers-color-scheme: dark)");
      const restyle = () => checkout.changeAppearance(appearance());
      scheme.addEventListener("change", restyle);

      const element = checkout.createPaymentElement({
        layout: "tabs",
        wallets: { applePay: "never", googlePay: "never", link: "never" },
      });
      current = {
        destroy() {
          scheme.removeEventListener("change", restyle);
          element.destroy();
        },
      };
      element.on("ready", () => { status.hidden = true; });
      element.on("loaderror", (event) => {
        status.hidden = true;
        showError((event.error && event.error.message) || "The card form could not load. Reload the page to try again.");
      });
      element.on("change", (event) => {
        complete = event.complete;
        const details = event.value && event.value.billingDetails;
        if (details && details.address && details.address.country) formCountry = details.address.country;
        if (actions) canConfirm = actions.getSession().canConfirm;
        update();
      });
      checkout.on("change", (session) => {
        show(session);
        update();
      });
      element.mount(panel.querySelector("[data-checkout-mount]"));

      const loaded = await checkout.loadActions();
      if (loaded.type === "error") throw new Error(loaded.error.message);
      actions = loaded.actions;
      show(actions.getSession());
      update();
    } catch (err) {
      status.hidden = true;
      showError(err.message || "The checkout could not be opened. Try again.");
      return;
    }

    form.addEventListener("submit", async (event) => {
      event.preventDefault();
      if (submit.disabled) return;
      showError("");
      setBusy(true, "Subscribing…");
      let result;
      try {
        result = await actions.confirm({ redirect: "if_required" });
      } catch (err) {
        result = { type: "error", error: err };
      }
      if (result.type === "error") {
        setBusy(false);
        showError((result.error && result.error.message) || "The payment did not go through. Try again.");
        return;
      }
      setBusy(true, "Confirming your subscription…");
      if (current) {
        current.destroy();
        current = null;
      }
      finish(panel, returnURL);
    });
  }

  function countryStep(form) {
    if (form.dataset.bound) return;
    form.dataset.bound = "true";
    const select = form.querySelector("[data-country-select]");
    const submit = form.querySelector("[data-country-submit]");
    const update = () => {
      const option = select.selectedOptions[0];
      const route = (option && option.dataset.route) || "";
      form.querySelectorAll("[data-route-note]").forEach((el) => { el.hidden = el.dataset.routeNote !== route; });
      submit.hidden = route === "unavailable";
    };
    select.addEventListener("change", update);
    update();
  }

  function scan(root) {
    if (root.matches && root.matches("[data-checkout]")) mount(root);
    if (root.querySelectorAll) root.querySelectorAll("[data-checkout]").forEach(mount);
    if (root.matches && root.matches("[data-country-step]")) countryStep(root);
    if (root.querySelectorAll) root.querySelectorAll("[data-country-step]").forEach(countryStep);
  }

  document.addEventListener("DOMContentLoaded", () => scan(document));
  document.addEventListener("htmx:load", (event) => scan(event.detail.elt));
})();
