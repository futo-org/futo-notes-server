// Drives the billing country step ([data-country-step]): as the country
// changes, it shows what the chosen country's route means (tax added on
// Stripe's page, or no web payment at all) and hides Checkout where it
// cannot be used. The server decides the route again when it is posted.
//
// Mounts our checkout in every [data-checkout] panel, on page load and
// whenever htmx swaps one in. Stripe's Payment Element, made without an
// intent for the plan's amount and currency, collects the card inside its
// own frames, styled with our theme, and offers the account's saved cards
// (the panel's session URL answers the CustomerSession secret). Our button
// turns the form into a ConfirmationToken and posts it to the pay URL; the
// server checks its billing country and pays, and answers either the page
// that shows the result or a 3-D Secure step, which Stripe.js handles
// before the page posts the subscription back for the server to settle.
// A declined card leaves the form up to try another.
//
// The card form's own Country field starts at the billing country chosen
// before checkout (data-country), and the button is held, with a message,
// while the field, or the saved card picked, names another. The server
// refuses such a payment anyway, before charging anything.
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

  // post sends a JSON body (or none) to url with the page's CSRF token, if
  // any, and returns the JSON answer, throwing its message on failure.
  async function post(url, body) {
    const headers = { Accept: "application/json" };
    const csrf = document.querySelector('meta[name="csrf-token"]');
    if (csrf) headers["X-CSRF-Token"] = csrf.content;
    if (body) headers["Content-Type"] = "application/json";
    let res;
    try {
      res = await fetch(url, {
        method: "POST", headers, credentials: "same-origin", body: body ? JSON.stringify(body) : undefined,
      });
    } catch {
      throw new Error("We couldn't reach FUTO Notes. Check your connection and try again.");
    }
    const answer = await res.json().catch(() => ({}));
    if (!res.ok) throw new Error(answer.message || answer.error || "The payment did not go through. Try again.");
    return answer;
  }

  function finish(panel, returnURL) {
    // Inside the dashboard, swap the result in place; elsewhere, load it.
    if (window.htmx && panel.closest("#page") && document.querySelector('meta[name="csrf-token"]')) {
      window.htmx.ajax("GET", returnURL, { target: "#page", swap: "innerHTML" });
    } else {
      window.location.assign(returnURL);
    }
  }

  // The billing country a Payment Element change event shows: the saved
  // card's, when one is picked, or the new card's Country field (which
  // Stripe.js reports under billingDetails.address, though its reference
  // shows billingDetails.country).
  function eventCountry(value) {
    if (!value) return undefined;
    const saved = value.payment_method;
    if (saved) return (saved.billing_details && saved.billing_details.address && saved.billing_details.address.country) || "";
    const details = value.billingDetails;
    if (!details) return undefined;
    return details.address ? details.address.country : details.country;
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

    let stripe = null;
    let elements = null;
    // The card fields are complete.
    let complete = false;
    let busy = false;
    // The card form's Country field, or the saved card's, as change events
    // report it.
    let formCountry = chosen;

    const showError = (message) => {
      errorBox.textContent = message;
      errorBox.hidden = !message;
    };
    const update = () => {
      const otherCountry = formCountry !== chosen;
      countryBox.hidden = !otherCountry;
      submit.disabled = busy || !elements || !complete || otherCountry;
      submit.setAttribute("aria-busy", String(busy));
    };
    const setBusy = (on, text) => {
      busy = on;
      if (on) label.textContent = text;
      else label.innerHTML = idleLabel;
      panel.toggleAttribute("data-busy", on);
      update();
    };

    if (current) {
      current.destroy();
      current = null;
    }
    try {
      stripe = window.Stripe(panel.dataset.publishableKey);
      const [session, faces] = await Promise.all([
        post(panel.dataset.sessionUrl),
        // Without Barlow the fields keep the fallback fonts.
        fonts().catch(() => []),
      ]);
      elements = stripe.elements({
        mode: "subscription",
        amount: Number(panel.dataset.amount),
        currency: panel.dataset.currency,
        allowedPaymentMethodTypes: ["card"],
        paymentMethodCreation: "manual",
        customerSessionClientSecret: session.client_secret,
        appearance: appearance(),
        fonts: faces,
        loader: "auto",
      });
      const scheme = window.matchMedia("(prefers-color-scheme: dark)");
      const restyle = () => elements.update({ appearance: appearance() });
      scheme.addEventListener("change", restyle);

      const element = elements.create("payment", {
        layout: "tabs",
        wallets: { applePay: "never", googlePay: "never", link: "never" },
        defaultValues: { billingDetails: { address: { country: chosen } } },
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
        const country = eventCountry(event.value);
        if (country !== undefined) formCountry = country;
        update();
      });
      element.mount(panel.querySelector("[data-checkout-mount]"));
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
      try {
        const submitted = await elements.submit();
        if (submitted.error) throw submitted.error;
        const made = await stripe.createConfirmationToken({
          elements,
          params: { return_url: new URL(panel.dataset.pageUrl, window.location.href).href },
        });
        if (made.error) throw made.error;
        let answer = await post(panel.dataset.payUrl, { confirmation_token: made.confirmationToken.id });
        if (answer.status === "requires_action") {
          setBusy(true, "Waiting for your bank…");
          // Whatever the bank says, the server settles the payment.
          await stripe.handleNextAction({ clientSecret: answer.client_secret });
          setBusy(true, "Subscribing…");
          answer = await post(panel.dataset.payUrl, { subscription: answer.subscription });
        }
        setBusy(true, "Confirming your subscription…");
        if (current) {
          current.destroy();
          current = null;
        }
        finish(panel, answer.return_url);
      } catch (err) {
        setBusy(false);
        showError((err && err.message) || "The payment did not go through. Try again.");
      }
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
