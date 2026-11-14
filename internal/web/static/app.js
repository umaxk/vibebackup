// vbackuper UI: progressive enhancement only.
(function () {
  "use strict";

  // ---- target type switch ------------------------------------------------
  var typeSelect = document.getElementById("target-type");
  if (typeSelect) {
    var syncType = function () {
      var isDir = typeSelect.value === "dir";
      document.querySelectorAll("[data-type-section]").forEach(function (el) {
        el.hidden = el.getAttribute("data-type-section") !== (isDir ? "dir" : "db");
      });
      var ssl = document.querySelector("[data-ssl]");
      if (ssl) ssl.hidden = typeSelect.value !== "postgres";

      var port = document.querySelector('input[name="db_port"]');
      if (port && !port.value) {
        port.value = typeSelect.value === "mysql" ? "3306" : "5432";
      }
    };
    typeSelect.addEventListener("change", syncType);
    syncType();
  }

  // ---- connection test ---------------------------------------------------
  var form = document.getElementById("target-form");
  var testBtn = document.getElementById("test-btn");
  if (form && testBtn) {
    testBtn.addEventListener("click", async function () {
      var out = document.getElementById("test-result");
      var fd = new FormData(form);
      var type = String(fd.get("type"));
      var payload = {
        id: Number(form.getAttribute("data-target-id") || 0),
        type: type,
        name: fd.get("name") || "",
        schedule: fd.get("schedule") || "",
        enabled: fd.get("enabled") === "1",
        retention_keep: Number(fd.get("retention_keep") || 0),
      };
      if (type === "dir") {
        payload.dir = {
          path: fd.get("dir_path") || "",
          exclude: String(fd.get("dir_exclude") || "")
            .split(/\r?\n/)
            .map(function (s) { return s.trim(); })
            .filter(Boolean),
        };
      } else {
        payload.database = {
          host: fd.get("db_host") || "",
          port: Number(fd.get("db_port") || 0),
          database: fd.get("db_name") || "",
          user: fd.get("db_user") || "",
          password: fd.get("db_password") || "",
          sslmode: fd.get("db_sslmode") || "",
        };
      }

      out.textContent = "Проверяю подключение…";
      out.className = "test-result";
      try {
        var res = await fetch("/api/test", {
          method: "POST",
          headers: { "Content-Type": "application/json" },
          body: JSON.stringify(payload),
        });
        var data = await res.json().catch(function () { return {}; });
        if (res.ok && data.ok) {
          out.textContent = "Подключение успешно";
          out.className = "test-result ok";
        } else {
          out.textContent = "Ошибка: " + (data.error || res.status);
          out.className = "test-result err";
        }
      } catch (e) {
        out.textContent = "Ошибка запроса: " + e;
        out.className = "test-result err";
      }
    });
  }

  // ---- site form ---------------------------------------------------------
  var siteForm = document.getElementById("site-form");
  if (siteForm) {
    var dirToggle = siteForm.querySelector('input[name="dir_enabled"]');
    var dbToggle = siteForm.querySelector('input[name="db_enabled"]');
    var siteDBType = document.getElementById("site-db-type");
    var siteSSL = siteForm.querySelector("[data-ssl]");
    var sitePort = siteForm.querySelector('input[name="db_port"]');

    var syncSite = function () {
      siteForm.querySelectorAll("[data-site-section]").forEach(function (el) {
        var part = el.getAttribute("data-site-section");
        el.hidden = part === "dir" ? !dirToggle.checked : !dbToggle.checked;
      });
      if (siteSSL) {
        siteSSL.hidden = !dbToggle.checked || !siteDBType || siteDBType.value !== "postgres";
      }
    };
    var syncPort = function () {
      if (sitePort && !sitePort.value) {
        sitePort.value = siteDBType && siteDBType.value === "mysql" ? "3306" : "5432";
      }
      syncSite();
    };
    dirToggle.addEventListener("change", syncSite);
    dbToggle.addEventListener("change", syncSite);
    if (siteDBType) siteDBType.addEventListener("change", syncPort);
    syncSite();

    var splitLines = function (value) {
      return String(value || "")
        .split(/\r?\n/)
        .map(function (s) { return s.trim(); })
        .filter(Boolean);
    };
    var partText = function (part) {
      if (!part) return "";
      if (part.ok) return "ок" + (part.info ? " (" + part.info + ")" : "");
      return "ошибка: " + (part.error || "неизвестная");
    };

    var siteTestBtn = document.getElementById("site-test-btn");
    if (siteTestBtn) {
      siteTestBtn.addEventListener("click", async function () {
        var out = document.getElementById("site-test-result");
        var fd = new FormData(siteForm);
        var id = Number(siteForm.getAttribute("data-site-id") || 0);
        var payload = {
          id: id,
          name: fd.get("name") || "",
          schedule: fd.get("schedule") || "",
          enabled: fd.get("enabled") === "1",
          retention_keep: Number(fd.get("retention_keep") || 0),
          dir: dirToggle.checked
            ? { path: fd.get("dir_path") || "", exclude: splitLines(fd.get("dir_exclude")) }
            : null,
          db_type: dbToggle.checked ? String(fd.get("db_type") || "") : "",
          database: dbToggle.checked
            ? {
                host: fd.get("db_host") || "",
                port: Number(fd.get("db_port") || 0),
                database: fd.get("db_name") || "",
                user: fd.get("db_user") || "",
                password: fd.get("db_password") || "",
                sslmode: fd.get("db_sslmode") || "",
              }
            : null,
        };

        out.textContent = "Проверяю подключение…";
        out.className = "test-result";
        try {
          var url = id ? "/api/sites/" + id + "/test" : "/api/sites/test";
          var res = await fetch(url, {
            method: "POST",
            headers: { "Content-Type": "application/json" },
            body: JSON.stringify(payload),
          });
          var data = await res.json().catch(function () { return {}; });
          var lines = [];
          if (data.dir) lines.push("Каталог: " + partText(data.dir));
          if (data.db) lines.push("База: " + partText(data.db));
          if (data.error) lines.push(data.error);
          if (!lines.length) lines.push(data.error || "Ошибка проверки (" + res.status + ")");
          out.textContent = lines.join("\n");
          out.className = "test-result " + (res.ok && data.ok ? "ok" : "err");
        } catch (e) {
          out.textContent = "Ошибка запроса: " + e;
          out.className = "test-result err";
        }
      });
    }
  }

  // ---- cron preview ------------------------------------------------------
  var schedule = document.getElementById("schedule");
  var preview = document.getElementById("cron-preview");
  if (schedule && preview) {
    var timer = null;
    var updatePreview = async function () {
      var value = schedule.value.trim();
      if (!value) {
        preview.textContent = "Пустое поле — только ручной запуск.";
        return;
      }
      try {
        var res = await fetch("/api/cron/preview", {
          method: "POST",
          headers: { "Content-Type": "application/json" },
          body: JSON.stringify({ schedule: value }),
        });
        var data = await res.json().catch(function () { return {}; });
        if (!res.ok) {
          preview.textContent = "Ошибка расписания: " + (data.error || res.status);
          return;
        }
        var runs = (data.next || []).map(function (d) {
          return new Date(d).toLocaleString("ru-RU");
        });
        preview.textContent = runs.length
          ? "Ближайшие запуски: " + runs.join("; ")
          : "Расписание не даёт ближайших запусков.";
      } catch (e) {
        preview.textContent = "Ошибка запроса: " + e;
      }
    };
    schedule.addEventListener("input", function () {
      clearTimeout(timer);
      timer = setTimeout(updatePreview, 400);
    });
    updatePreview();
  }

  // ---- destructive forms ------------------------------------------------
  document.querySelectorAll("form[data-confirm]").forEach(function (f) {
    f.addEventListener("submit", function (e) {
      if (!window.confirm(f.getAttribute("data-confirm"))) {
        e.preventDefault();
      }
    });
  });

  // ---- auto refresh while a backup is running ---------------------------
  if (document.getElementById("live")) {
    setTimeout(function () { window.location.reload(); }, 8000);
  }
})();
