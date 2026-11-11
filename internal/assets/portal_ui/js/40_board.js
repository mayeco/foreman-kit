'use strict';
// ---------------------------------------------------------------------------
// The board: a card a person can move is dragged to a lane, and dropping it
// there presses the button of the card that moves it to that lane. The button
// is what acts: it asks what it asks before acting (a move that is not undone
// is confirmed), posts the action and brings the lanes up to date. A card has
// a button for each lane it can go to, so the board is used the same way with
// the keyboard and on a phone, where nothing is dragged.
// ---------------------------------------------------------------------------

const Board = { card: null };

// The button of a card that moves it to a lane, if it has one.
function moveButton(card, lane) {
  return $$('[data-move-to]', card).find((b) => b.dataset.moveTo === lane.dataset.lane) || null;
}

// While a card is dragged, each lane says whether the card can be dropped on it: on the lanes one
// of its buttons leads to, except the lane it is already in.
function markLanes(card) {
  $$('[data-lane]').forEach((lane) => {
    delete lane.dataset.over;
    if (!card) { delete lane.dataset.drop; return; }
    lane.dataset.drop = lane !== card.closest('[data-lane]') && moveButton(card, lane) ? 'yes' : 'no';
  });
  document.body.toggleAttribute('data-dragging', !!card);
}

// The lane under the pointer, when the card being dragged can be dropped on it.
function dropLane(ev) {
  const lane = Board.card && ev.target.closest?.('[data-lane]');
  return lane && lane.dataset.drop === 'yes' ? lane : null;
}

document.addEventListener('dragstart', (ev) => {
  const card = ev.target.closest?.('[data-card][draggable="true"]');
  if (!card) return;
  Board.card = card;
  ev.dataTransfer.effectAllowed = 'move';
  ev.dataTransfer.setData('text/plain', card.dataset.card);
  markLanes(card);
});

function overLane(ev) {
  if (!Board.card) return;
  const lane = dropLane(ev);
  $$('[data-lane][data-over]').forEach((l) => { if (l !== lane) delete l.dataset.over; });
  if (!lane) return;
  // Only a lane that takes the card cancels the event, which is what lets the card be dropped.
  ev.preventDefault();
  ev.dataTransfer.dropEffect = 'move';
  lane.dataset.over = 'yes';
}
document.addEventListener('dragenter', overLane);
document.addEventListener('dragover', overLane);

document.addEventListener('drop', (ev) => {
  const card = Board.card;
  const lane = dropLane(ev);
  if (!card || !lane) return;
  ev.preventDefault();
  const button = moveButton(card, lane);
  Board.card = null;
  markLanes(null);
  if (button) runButton(button);
});

document.addEventListener('dragend', () => {
  Board.card = null;
  markLanes(null);
});
