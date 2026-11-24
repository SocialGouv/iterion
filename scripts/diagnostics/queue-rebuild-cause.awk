# Why each merge-queue rebuild happened, read from the group's BASE.
#
# Driven by scripts/diagnostics/queue-stats.sh, which hands it one row per
# `merge_group` build of the Tests workflow, tab-separated, SORTED BY CREATION:
#
#   1 created (epoch)        6 updated (epoch)
#   2 pull request           7 status
#   3 base kind              8 inside the window (1) or on the day before (0)
#   4 outcome                9 base sha
#   5 run id                10 base commit date (epoch), "-" when the base is
#                              not a commit on main (a speculative queue branch)
#
# A row whose pull request already had a build REPLACES that build. The CAUSE
# is read from the new group's base, which the branch name carries: GitHub
# re-forms a group on main's new tip, so a base that MOVED to a release, a
# direct merge or any other push IS the push that discarded the previous
# build. No clock is involved.
#
# What that replaces, and why: the previous rule blamed a push only when its
# commit date fell within 90 seconds of the rebuild. GitHub rebuilt #1967's
# group 114 seconds after v3.217.0's commit (06:30:01Z -> 06:31:55Z) and the
# rebuild was counted as the queue's own — an undercount of exactly the line
# the release hold exists to shrink. A base sha is a fact the group carries;
# "within 90 seconds" was a guess about GitHub's scheduling latency.
#
# A base that did NOT move (same sha) is the queue re-forming the same group:
# an entry ahead failed or left. A base that moved to a queue merge, or to a
# speculative queue branch, is the queue's own progress too. Both read as
# `queue`, as does a dequeue followed by a re-enqueue.
#
# UNLESS the replaced build had already finished RED before main moved: that
# build had ejected its pull request, so its rebuild is a re-enqueue the queue
# owns whatever the new base is.
#
# The STATE of the replaced build is read at the moment main MOVED when a
# push caused the rebuild — a build that finished in the seconds between the
# push and the rebuild was still running when the push hit it — and at the
# rebuild otherwise: green, red, cancelled, or still running (GitHub does not
# cancel the run of a group it discards; .github/workflows/merge-queue-churn.yml
# does).
#
# That moment is the base commit's date, which stands in for the push time: a
# push lands seconds after its commit, but a local series pushed long after it
# was made would place the moment too early. Only this reading depends on that
# approximation — the CAUSE no longer does.
#
# The order of the rows IS the fact "which build this one replaced", so it is
# verified rather than trusted: a row older than the row before it stops the
# program instead of producing a report that reads like a measurement. The
# caller sorts by creation and breaks ties by run id (GitHub's run ids grow),
# so the verdict is a function of the data, not of the answer order.

BEGIN { FS = "\t" }

NR > 1 && $1 + 0 < previous_created + 0 {
	printf "queue-rebuild-cause: row %d (run %s, created %s) is older than the row before it (%s) — this program reads which build a rebuild replaced from the row order, so it refuses input the caller has not sorted by creation\n", NR, $5, $1, previous_created > "/dev/stderr"
	unsorted = 1
	exit 2
}

{
	previous_created = $1
	# $8 == 0 is the day before the window: such a row is a predecessor the
	# attribution needs, never a rebuild the window paid for.
	if (($2 in pcreated) && $8 == 1) {
		moved = ($9 != pbase[$2]) && ($3 == "release" || $3 == "direct-merge" || $3 == "other-push")
		cause = moved ? $3 : "queue"
		# "-" cannot happen on a push base (it is a commit on main), but a
		# missing date must not silently read as epoch 0.
		when = ($10 == "-") ? $1 : $10
		finished = (pstatus[$2] == "completed")
		if (cause != "queue" && finished && pupdated[$2] <= when && pout[$2] != "success") cause = "queue"
		at = (cause == "queue") ? $1 : when
		state = (finished && pupdated[$2] <= at) ? pout[$2] : "in-flight"
		r[state " -> " cause]++
		if (cause != "queue") {
			lost++
			bykind[cause]++
			if (state == "success") green++; else running++
		}
	}
	pcreated[$2] = $1
	pupdated[$2] = $6
	pstatus[$2] = $7
	pout[$2] = $4
	pbase[$2] = $9
}

END {
	# awk runs END even on `exit`: a refused input prints no report.
	if (unsorted) exit 2
	print "Rebuilds — state of the replaced build -> cause:"
	for (x in r) printf "  %-44s %d\n", x, r[x]
	printf "Queue builds discarded by a direct push to main: %d (already green %d, still running %d); pushes: release %d, direct merge %d, other %d\n",
		lost, green, running, bykind["release"], bykind["direct-merge"], bykind["other-push"]
}
