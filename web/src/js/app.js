import Alpine from "@alpinejs/csp";
import htmx from "htmx.org";
import {
  ChevronDown,
  CircleCheckBig,
  DatabaseZap,
  FileArchive,
  FileSpreadsheet,
  Landmark,
  createIcons,
  LayoutDashboard,
  LogOut,
  Menu,
  Monitor,
  Moon,
  PanelLeftClose,
  ScrollText,
  Shield,
  ShieldCheck,
  Sun,
  UserRound,
  Users,
  UserShield,
  X,
} from "lucide";

window.Alpine = Alpine;
window.htmx = htmx;
htmx.config.allowEval = false;
htmx.config.allowScriptTags = false;
htmx.config.includeIndicatorStyles = false;

Alpine.data("noticeToast", () => ({
  open: true,
  init() { setTimeout(() => { this.open = false; }, 5000); },
}));

const root = document.documentElement;
const systemTheme = window.matchMedia("(prefers-color-scheme: dark)");
const sidebarDisclosures = window.__sidebarDisclosures || Object.create(null);

const applyTheme = (preference) => {
  const dark = preference === "dark" || (preference === "system" && systemTheme.matches);
  root.dataset.themePreference = preference;
  root.classList.toggle("dark", dark);
  root.style.colorScheme = dark ? "dark" : "light";
};

Alpine.store("theme", {
  preference: root.dataset.themePreference || "system",
  init() {
    systemTheme.addEventListener("change", () => {
      if (this.preference === "system") applyTheme("system");
    });
  },
  set(preference) {
    if (!["light", "dark", "system"].includes(preference)) preference = "system";
    this.preference = preference;
    try {
      localStorage.setItem("theme", preference);
    } catch {}
    applyTheme(preference);
  },
});

Alpine.data("adminShell", () => ({
  sidebarCollapsed: root.dataset.sidebarCollapsed === "true",
  mobileOpen: false,
  init() {
    root.dataset.mobileSidebarOpen = "false";
    this.$watch("mobileOpen", (open) => {
      root.dataset.mobileSidebarOpen = String(open);
      document.body.classList.toggle("overflow-hidden", open);
    });
  },
  toggleSidebar() {
    this.sidebarCollapsed = !this.sidebarCollapsed;
    root.dataset.sidebarCollapsed = String(this.sidebarCollapsed);
    try {
      localStorage.setItem("sidebar-collapsed", String(this.sidebarCollapsed));
    } catch {}
  },
  expandSidebar() {
    if (!this.sidebarCollapsed) return;
    this.sidebarCollapsed = false;
    root.dataset.sidebarCollapsed = "false";
    try {
      localStorage.setItem("sidebar-collapsed", "false");
    } catch {}
  },
  closeMobile() {
    this.mobileOpen = false;
  },
}));

Alpine.data("navigationDisclosure", () => ({
  key: "",
  active: false,
  manualOpen: false,
  init() {
    this.key = this.$el.dataset.navigationKey;
    this.active = this.$el.dataset.navigationActive === "true";
    this.manualOpen = this.$el.dataset.navigationManualOpen === "true";
  },
  get open() {
    return this.active || this.manualOpen;
  },
  togglePrimary() {
    if (this.sidebarCollapsed) { this.expandSidebar(); this.openDisclosure(); }
    else this.toggleDisclosure();
  },
  openDisclosure() {
    if (this.active || this.manualOpen) return;
    this.manualOpen = true;
    this.persistDisclosure();
  },
  toggleDisclosure() {
    if (this.active) return;
    this.manualOpen = !this.manualOpen;
    this.persistDisclosure();
  },
  persistDisclosure() {
    sidebarDisclosures[this.key] = this.manualOpen;
    try {
      localStorage.setItem("sidebar-disclosures", JSON.stringify(sidebarDisclosures));
    } catch {}
  },
}));

Alpine.data("confirmationDialog", () => ({
  form: null,
  trigger: null,
  title: "Confirm action",
  message: "Are you sure?",
  confirmLabel: "Confirm",
  open(event) {
    this.form = event.detail.form;
    this.trigger = document.getElementById(this.form.dataset.confirmCancelFocus || "") || event.detail.trigger;
    this.title = this.form.dataset.confirmTitle || "Confirm action";
    this.message = this.form.dataset.confirmMessage || "Are you sure?";
    this.confirmLabel = this.form.dataset.confirmLabel || "Confirm";
    this.$refs.dialog.showModal();
    this.$nextTick(() => this.$refs.cancel.focus());
  },
  cancel() {
    this.$refs.dialog.close();
  },
  confirm() {
    const form = this.form;
    const successFocus = document.getElementById(form.dataset.confirmSuccessFocus || "");
    if (successFocus) this.trigger = successFocus;
    this.$refs.dialog.close();
    form.dataset.confirmed = "true";
    form.requestSubmit();
    queueMicrotask(() => delete form.dataset.confirmed);
  },
  restoreFocus() {
    this.trigger?.focus();
    this.form = null;
    this.trigger = null;
  },
}));

document.addEventListener("submit", (event) => {
  const form = event.target;
  if (!(form instanceof HTMLFormElement) || !form.matches("[data-confirm]") || form.dataset.confirmed === "true") return;
  event.preventDefault();
  window.dispatchEvent(new CustomEvent("confirm-submit", { detail: { form, trigger: event.submitter } }));
});

const initializeIcons = () =>
  createIcons({
    icons: { ChevronDown, CircleCheckBig, DatabaseZap, FileArchive, FileSpreadsheet, Landmark, LayoutDashboard, LogOut, Menu, Monitor, Moon, PanelLeftClose, ScrollText, Shield, ShieldCheck, Sun, UserRound, UserShield, Users, X },
  });

// Icons must exist before Alpine binds: Lucide replaces <i> with a new <svg>
// carrying over Alpine-added classes (e.g. rotate-180) that Alpine then never
// removes. The script is deferred, so the DOM is already parsed here.
initializeIcons();
document.body.addEventListener("htmx:afterSwap", initializeIcons);

Alpine.start();
