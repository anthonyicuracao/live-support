// js/ui-lib.js — small self-contained UI primitives, ported from
// hy-rag/public/js-lib.js (same family as window.getInput there).
//
// window.commandBox(message, options?, defaultOptionIndex?, icon?, container?, timeoutSeconds?)
//   Displays a modal dialog and resolves the selected option's hotkey letter,
//   or false when dismissed (close ×, Escape, or timeout). Options are labels;
//   prefix a character with '&' to set its hotkey (default hotkey = first
//   letter). Arrow keys navigate, Enter selects, hotkeys select directly.
//
//   Extensions over the hy-rag original:
//   - timeoutSeconds: auto-dismiss (resolves false) after N seconds — for
//     prompts that must not outlive their moment (e.g. anything tied to a
//     ringing call).
//   - The returned promise carries a .close(result) method so code can
//     dismiss the box when the situation changes (caller hung up, state
//     reset, etc.). result defaults to false.
(function () {
  window.commandBox = function commandBox(message, options, defaultOptionIndex, icon, container, timeoutSeconds) {
    let closeFn = null;
    const p = new Promise((resolve) => {
      options = Array.isArray(options) && options.length ? options : ["Ok", "Cancel"];
      defaultOptionIndex =
        typeof defaultOptionIndex === "number" && defaultOptionIndex >= 1 && defaultOptionIndex <= options.length
          ? defaultOptionIndex
          : 1;
      icon = typeof icon === "string" ? icon : null;
      const parent = container instanceof Element ? container : document.body;

      // Brightness-aware palette: match the page rather than impose a theme.
      const bodyBg = window.getComputedStyle(parent).backgroundColor;
      const rgb = bodyBg.match(/\d+/g);
      const [r, g, b] = rgb ? rgb.map(Number) : [255, 255, 255];
      const isDark = 0.2126 * r + 0.7152 * g + 0.0722 * b < 128;

      const overlayBg = isDark ? "rgba(0,0,0,0.7)" : "rgba(0,0,0,0.5)";
      const dialogBg = isDark ? "#2c2c2c" : "#ffffff";
      const textColor = isDark ? "#e0e0e0" : "#212529";
      const btnBg = isDark ? "#444444" : "#f0f0f0";
      const btnText = isDark ? "#ffffff" : "#212529";

      function lightenColor(hex, pct) {
        const num = parseInt(hex.slice(1), 16);
        let rr = (num >> 16) + Math.round(2.55 * pct);
        let gg = ((num >> 8) & 0xff) + Math.round(2.55 * pct);
        let bb = (num & 0xff) + Math.round(2.55 * pct);
        rr = Math.min(255, Math.max(0, rr));
        gg = Math.min(255, Math.max(0, gg));
        bb = Math.min(255, Math.max(0, bb));
        return "#" + ((1 << 24) + (rr << 16) + (gg << 8) + bb).toString(16).slice(1);
      }

      const defaultBtnBg = lightenColor(dialogBg, isDark ? 20 : -30);
      const lumD = (() => {
        const num = parseInt(defaultBtnBg.slice(1), 16);
        const R = num >> 16, G = (num >> 8) & 0xff, B = num & 0xff;
        return 0.2126 * R + 0.7152 * G + 0.0722 * B;
      })();
      const defaultBtnText = lumD < 128 ? "#ffffff" : "#212529";

      const parsed = options.map((o, i) => {
        const idx = o.indexOf("&");
        const hot = idx >= 0 ? o[idx + 1] : o[0];
        return { display: o.replace("&", ""), hotkey: hot.toUpperCase(), index: i + 1 };
      });

      const overlay = document.createElement("div");
      Object.assign(overlay.style, {
        position: "fixed", top: "0", left: "0", width: "100%", height: "100%",
        backgroundColor: overlayBg,
        display: "flex", alignItems: "center", justifyContent: "center", zIndex: "99999",
      });

      const dialog = document.createElement("div");
      Object.assign(dialog.style, {
        position: "relative", backgroundColor: dialogBg, color: textColor,
        padding: "24px 28px", borderRadius: "10px", minWidth: "320px", maxWidth: "90%",
        boxShadow: isDark ? "0 6px 18px rgba(0,0,0,0.6)" : "0 6px 18px rgba(0,0,0,0.15)",
        fontFamily: "Segoe UI, sans-serif", lineHeight: "1.5",
      });

      const closeBtn = document.createElement("button");
      closeBtn.innerHTML = "&times;";
      Object.assign(closeBtn.style, {
        position: "absolute", top: "8px", right: "8px", background: "transparent",
        border: "none", color: textColor, fontSize: "24px", cursor: "pointer", padding: "4px",
        transition: "filter 0.2s, transform 0.2s",
      });
      closeBtn.addEventListener("mouseenter", () => { closeBtn.style.filter = "brightness(1.3)"; closeBtn.style.transform = "scale(1.3)"; });
      closeBtn.addEventListener("mouseleave", () => { closeBtn.style.filter = "none"; closeBtn.style.transform = "none"; });
      closeBtn.addEventListener("click", () => { cleanup(); resolve(false); });
      dialog.appendChild(closeBtn);

      const msgC = document.createElement("div");
      Object.assign(msgC.style, { display: "flex", alignItems: "center", marginTop: "32px", marginBottom: "20px" });
      if (icon) {
        const ic = document.createElement("span");
        Object.assign(ic.style, { fontSize: "30px", marginRight: "14px" });
        ic.textContent =
          icon === "EXCLAMATION" ? "❗" : icon === "HAND" ? "✋" : icon === "QUESTION" ? "❓" : icon === "INFO" ? "ℹ️" : icon;
        msgC.appendChild(ic);
      }
      const txt = document.createElement("div");
      txt.textContent = message;
      msgC.appendChild(txt);
      dialog.appendChild(msgC);

      const cols = parsed.length === 4 ? 2 : Math.min(3, parsed.length);
      const btnC = document.createElement("div");
      Object.assign(btnC.style, { display: "grid", gridTemplateColumns: `repeat(${cols}, auto)`, gap: "16px", justifyContent: "center" });
      const btnEls = [];

      parsed.forEach(({ display, hotkey, index }) => {
        const isDef = index === defaultOptionIndex;
        const baseBg = isDef ? defaultBtnBg : btnBg;
        const baseText = isDef ? defaultBtnText : btnText;
        const hoverBg = lightenColor(baseBg, 15);

        const btn = document.createElement("button");
        btn.type = "button";
        Object.assign(btn.style, {
          padding: "10px 20px", border: "2px solid transparent", borderRadius: "6px",
          cursor: "pointer", fontSize: "15px", outline: "none",
          backgroundColor: baseBg, color: baseText,
          transition: "background-color 0.2s, box-shadow 0.2s, border-color 0.2s",
        });
        if (isDef) btn.autofocus = true;

        const pos = display.toUpperCase().indexOf(hotkey);
        if (pos >= 0) {
          btn.innerHTML = `${display.slice(0, pos)}<span style="text-decoration:underline">${display[pos]}</span>${display.slice(pos + 1)}`;
        } else {
          btn.textContent = display;
        }

        btn.addEventListener("mouseenter", () => { if (isDark) btn.style.backgroundColor = hoverBg; else btn.style.borderColor = lightenColor(baseBg, -30); });
        btn.addEventListener("mouseleave", () => { if (isDark) btn.style.backgroundColor = baseBg; else btn.style.borderColor = "transparent"; });
        btn.addEventListener("focus", () => (btn.style.boxShadow = `0 0 0 0.2rem ${lightenColor(baseBg, 30)}`));
        btn.addEventListener("blur", () => (btn.style.boxShadow = "none"));
        btn.addEventListener("click", () => selectOption(index));

        btnC.appendChild(btn);
        btnEls.push(btn);
      });
      dialog.appendChild(btnC);
      overlay.appendChild(dialog);
      parent.appendChild(overlay);

      let currentFocus = defaultOptionIndex - 1;
      if (btnEls[currentFocus]) btnEls[currentFocus].focus();

      // Arrow-key navigation: left/right within a row, up/down between rows.
      const keyH = (e) => {
        if (e.key === "Escape") {
          e.preventDefault(); cleanup(); resolve(false);
        } else if (e.key === "ArrowRight") {
          e.preventDefault();
          if (currentFocus + 1 < btnEls.length && (currentFocus + 1) % cols !== 0) currentFocus += 1;
          btnEls[currentFocus].focus();
        } else if (e.key === "ArrowLeft") {
          e.preventDefault();
          if (currentFocus % cols !== 0) currentFocus -= 1;
          btnEls[currentFocus].focus();
        } else if (e.key === "ArrowDown") {
          e.preventDefault();
          if (currentFocus + cols < btnEls.length) { currentFocus += cols; btnEls[currentFocus].focus(); }
        } else if (e.key === "ArrowUp") {
          e.preventDefault();
          if (currentFocus - cols >= 0) { currentFocus -= cols; btnEls[currentFocus].focus(); }
        } else if (e.key === "Enter") {
          e.preventDefault(); selectOption(currentFocus + 1);
        } else {
          const k = e.key.toUpperCase();
          parsed.forEach(({ hotkey, index }) => {
            if (k === hotkey) { e.preventDefault(); selectOption(index); }
          });
        }
      };
      document.addEventListener("keydown", keyH);

      // Auto-dismiss: a modal tied to a moment (e.g. a ringing call) must not
      // outlive it. Resolves false, same as an explicit dismissal.
      let timer = null;
      if (typeof timeoutSeconds === "number" && timeoutSeconds > 0) {
        timer = setTimeout(() => { cleanup(); resolve(false); }, timeoutSeconds * 1000);
      }

      function selectOption(i) { cleanup(); resolve(parsed[i - 1].hotkey); }
      function cleanup() {
        document.removeEventListener("keydown", keyH);
        if (timer) clearTimeout(timer);
        overlay.remove();
      }
      closeFn = (result) => { cleanup(); resolve(result === undefined ? false : result); };
    });
    // External dismissal — code can close the box when the situation changes.
    p.close = (result) => { if (closeFn) closeFn(result); else console.warn("[commandBox] close() before init — ignored"); return p; };
    return p;
  };
})();
