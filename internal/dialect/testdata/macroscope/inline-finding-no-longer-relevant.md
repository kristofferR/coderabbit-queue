<!-- MURMUR_IGNORE -->
🟡 **Medium** `home/homeThreadList.ts:105`

`buildHomeProjectScopes` picks `projects[0]` as the representative, so when a grouped project has no thread or pending activity, `sortHomeProjectScopes` sorts it by whichever member happened to appear first in the input array rather than by the newest `createdAt`/`updatedAt` across all members. A repo grouped across two machines where the older one is listed first will always sort below a peer whose representative is newer, even if the repo's other member was updated more recently. Consider selecting the member with the freshest `createdAt`/`updatedAt` timestamp (per `projectSortOrder`) as the representative, or have `sortHomeProjectScopes` compute its project-level fallback across all `scope.projects`.

No longer relevant as of 6a06232237270dfc6d1e39af9611ce2ac3349ce5