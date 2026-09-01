// Progressive enhancement only.
//
// Every control on this page works without JavaScript: the filters are a form
// with a submit button, and the destructive actions are ordinary form posts.
// This file removes two rough edges for people who do have it — nothing here is
// load-bearing, and the page is fully usable if it fails to load.
(function () {
  'use strict';

  // Auto-submit the filter form when a select changes, and hide the button that
  // exists for people without scripting.
  document.querySelectorAll('form[data-autosubmit]').forEach(function (form) {
    form.querySelectorAll('select').forEach(function (select) {
      select.addEventListener('change', function () { form.submit(); });
    });
    form.querySelectorAll('[data-nojs]').forEach(function (el) { el.hidden = true; });
  });

  // Confirm actions that cannot be undone from the page they are on.
  //
  // Without scripting the form still posts — untracking keeps the history, so
  // the worst case is a click that is easy to reverse by adding the video back.
  document.querySelectorAll('form[data-confirm]').forEach(function (form) {
    form.addEventListener('submit', function (event) {
      if (!window.confirm(form.getAttribute('data-confirm'))) {
        event.preventDefault();
      }
    });
  });
})();
