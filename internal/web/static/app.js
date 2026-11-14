// vbackuper UI: progressive enhancement only.
(function () {
  "use strict";

  // ---- project form ------------------------------------------------------
  var projectForm = document.getElementById("project-form");
  if (projectForm) {
    var dirToggle = projectForm.querySelector('input[name="dir_enabled"]');
    var dbToggle = projectForm.querySelector('input[name="db_enabled"]');
    var projectDBType = document.getElementById("project-db-type");
    var projectSSL = projectForm.querySelector("[data-ssl]");
    var projectPort = projectForm.querySelector('input[name="db_port"]');

    var syncProject = function () {
      projectForm.querySelectorAll("[data-project-section]").forEach(function (el) {
        var part = el.getAttribute("data-project-section");
        el.hidden = part === "dir" ? !dirToggle.checked : !dbToggle.checked;
      });
      if (projectSSL) {
        projectSSL.hidden = !dbToggle.checked || !projectDBType || projectDBType.value !== "postgres";
      }
    };
    var syncPort = function () {
      if (projectPort && !projectPort.value) {
        projectPort.value = projectDBType && projectDBType.value === "mysql" ? "3306" : "5432";
      }
      syncProject();
    };
    dirToggle.addEventListener("change", syncProject);
    dbToggle.addEventListener("change", syncProject);
    if (projectDBType) projectDBType.addEventListener("change", syncPort);
    syncProject();

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

    var projectTestBtn = document.getElementById("project-test-btn");
    if (projectTestBtn) {
      projectTestBtn.addEventListener("click", async function () {
        var out = document.getElementById("project-test-result");
        var fd = new FormData(projectForm);
        var id = Number(projectForm.getAttribute("data-project-id") || 0);
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
          var url = id ? "/api/projects/" + id + "/test" : "/api/projects/test";
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
