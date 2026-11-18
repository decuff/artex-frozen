import { type ClassValue, clsx } from "clsx";
import { twMerge } from "tailwind-merge";

export function cn(...inputs: ClassValue[]) {
  return twMerge(clsx(inputs));
}

// Helper for onInteractOutside close decision in Sheet/Dialog.
//
// Background: Radix popovers inside a drawer (Select dropdown, DropdownMenu, Popover, etc.) portal
// outside the drawer. Clicking the overlay/outside the drawer while a popover is open to dismiss it — that pointerdown gets handled by both Select and Sheet
// DismissableLayer; Select closes first and it's a discrete event, so React flushes synchronously, and thus
// by the time Sheet's handler runs, the popover's data-state has already flipped to closed — detecting whether the popover is open "at the moment"
// is inherently unreliable (verified empirically).
//
// Correct approach: Radix's pointerdown listener runs in the bubble phase; we record in the capture phase (earlier)
// "whether a popover is open right now," and onInteractOutside reads this recorded value to decide whether to allow the close.
function isRadixOverlayOpenNow(): boolean {
  if (typeof document === "undefined") return false;
  return !!document.querySelector(
    [
      "[data-slot='select-trigger'][data-state='open']",
      "[data-slot='select-content'][data-state='open']",
      "[role='listbox'][data-state='open']",
      "[data-radix-popper-content-wrapper]",
      "[aria-expanded='true'][data-state='open']",
    ].join(","),
  );
}

let overlayOpenAtLastPointerDown = false;
if (typeof document !== "undefined") {
  document.addEventListener(
    "pointerdown",
    () => {
      overlayOpenAtLastPointerDown = isRadixOverlayOpenNow();
    },
    true, // Capture before the Radix pointerdown handler runs in the bubble phase.
  );
}

// radixOverlayWasOpenAtPointerDown returns "whether a Radix popover was
// open when the most recent pointerdown occurred." The drawer/dialog uses this: clicking the overlay while a popover is open → dismiss only the popover, not itself.
export function radixOverlayWasOpenAtPointerDown(): boolean {
  return overlayOpenAtLastPointerDown;
}

// copyText writes text to the clipboard and returns whether it succeeded.
// Background: navigator.clipboard is only available in secure contexts (HTTPS / localhost); via IP + HTTP
// access it's undefined, so fall back to execCommand("copy").
export async function copyText(text: string): Promise<boolean> {
  if (navigator.clipboard && window.isSecureContext) {
    try {
      await navigator.clipboard.writeText(text);
      return true;
    } catch {
      // Fall back to the alternative copy method.
    }
  }
  try {
    const textarea = document.createElement("textarea");
    textarea.value = text;
    textarea.style.position = "fixed";
    textarea.style.left = "-9999px";
    textarea.style.top = "0";
    document.body.appendChild(textarea);
    textarea.focus();
    textarea.select();
    const ok = document.execCommand("copy");
    document.body.removeChild(textarea);
    return ok;
  } catch {
    return false;
  }
}

export const getInitials = (str: string): string => {
  if (typeof str !== "string" || !str.trim()) return "?";

  return (
    str
      .trim()
      .split(/\s+/)
      .filter(Boolean)
      .map((word) => word[0])
      .join("")
      .toUpperCase() || "?"
  );
};

export function formatCurrency(
  amount: number,
  opts?: {
    currency?: string;
    locale?: string;
    minimumFractionDigits?: number;
    maximumFractionDigits?: number;
    noDecimals?: boolean;
  },
) {
  const { currency = "USD", locale = "en-US", minimumFractionDigits, maximumFractionDigits, noDecimals } = opts ?? {};

  const formatOptions: Intl.NumberFormatOptions = {
    style: "currency",
    currency,
    minimumFractionDigits: noDecimals ? 0 : minimumFractionDigits,
    maximumFractionDigits: noDecimals ? 0 : maximumFractionDigits,
  };

  return new Intl.NumberFormat(locale, formatOptions).format(amount);
}
