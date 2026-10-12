import { create } from "zustand";
import { syncVisoraUser } from "../../lib/visora";

const TOKEN_KEY = "resin_admin_token";

function loadInitialToken(): string {
  if (typeof window === "undefined") {
    return "";
  }
  return window.localStorage.getItem(TOKEN_KEY) ?? "";
}

function syncVisoraIdentity(token: string) {
  if (token.trim()) {
    syncVisoraUser({ id: "admin", name: "管理员" });
    return;
  }
  syncVisoraUser(null);
}

type AuthState = {
  token: string;
  setToken: (token: string) => void;
  clearToken: () => void;
};

const initialToken = loadInitialToken();
syncVisoraIdentity(initialToken);

export const useAuthStore = create<AuthState>((set) => ({
  token: initialToken,
  setToken: (token) => {
    const next = token.trim();
    if (typeof window !== "undefined") {
      window.localStorage.setItem(TOKEN_KEY, next);
    }
    syncVisoraIdentity(next);
    set({ token: next });
  },
  clearToken: () => {
    if (typeof window !== "undefined") {
      window.localStorage.removeItem(TOKEN_KEY);
    }
    syncVisoraIdentity("");
    set({ token: "" });
  },
}));

export function getStoredAuthToken(): string {
  return useAuthStore.getState().token;
}
