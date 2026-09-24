# The coaching routine

Twice a day, for the 20 days of the [study plan](00-study-plan.md), Claude checks in, quizzes you, and keeps your record in [`progress/`](../progress/README.md). This page is the contract for those check-ins, so both of you know what to expect.

**Schedule:** 10:00 and 22:00 IST (04:30 and 16:30 UTC), Thursday 24 September (F0 setup check) through Wednesday 14 October (Day 20). Day _n_ is 24 September + _n_ days.

## Morning check-in (10:00 IST, about 20 minutes)

1. **Warm-up (5 questions).** Due items from [weak-topics](../progress/weak-topics.md) first, then questions on yesterday's material. Answer in your own words, briefly, without notes. Short and honest beats long and copied.
2. **Feedback.** Each answer is scored 2 / 1 / 0 with a one-line correction or the missing piece.
3. **Today's brief.** The day's plan from the study plan, adjusted for anything carried over, with the one idea that matters most today and the lab result you should be able to show tonight.

## Evening check-in (22:00 IST, about 30–40 minutes)

1. **Report.** What you finished, hours studied, what surprised you in the lab, what you are stuck on. Paste lab output or notebook lines if useful.
2. **Unblock.** Anything you are stuck on gets explained first.
3. **Quiz (6–8 questions).** The day's "Check yourself" questions plus follow-ups that go one level deeper ("why?", "what breaks if…?", "how does Roundhouse do it?"). From day 6 on, at least one question asks you to explain a specific piece of Roundhouse code. From day 13 on, at least one is an interview-style design question.
4. **Scoring and record.** Claude updates the tracker (status, hours, score), adds missed topics to the review queue, adjusts readiness ratings with evidence, writes `progress/log/day-NN.md`, and commits the changes to the repository.
5. **Tomorrow.** The plan for tomorrow, including any carry-over.

## Rules Claude follows

- **Ask, don't lecture.** Questions come first; explanations follow your answer.
- **No free points.** Vague answers score 1, not 2. "It isolates things" is not an explanation of a namespace.
- **Always the next level down.** A correct answer is followed by "and what happens underneath?" until you reach something you cannot explain. That edge is where the next study time goes.
- **Spaced review.** Missed topics come back after 1, 3 and 7 days until you answer them well three times in a row.
- **Adapt the plan.** Behind schedule: drop items marked (opt) and use the day 7 and 14 buffers before cutting core content. Ahead: add exercises. Consistently scoring above 85%: questions get harder.
- **Honest readiness.** Ratings reflect what you demonstrated, not what you read. If an area is not ready by day 18, you will hear it on day 18, not on interview day.
- **Days 19 and 20** are mock interviews: Claude plays the interviewer for the full 45-minute design prompt, interrupts with follow-ups, and scores the result against the checkpoints in [module 10](10-interview-prep.md).

## If you miss a check-in

Nothing is lost. Reply whenever you can and say which day you are on; the next check-in starts from the tracker, not from the calendar. If you need a rest day, say so, and the plan shifts by a day.

## Changing the routine

Ask in the conversation at any time: different times, one check-in instead of two, a pause, or more mock interviews. When the 20 days are over, the routine ends; ask to extend it into the "more than 20 days" plan if you want to continue.
