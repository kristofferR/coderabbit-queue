### Queue cap desyncs stores

**High Severity**

<!-- DESCRIPTION START -->
The 50-message queue limit is only enforced in the projector's in-memory state. The decider and SQL persistence still accept and store more messages, causing a divergence between the capped read model and the uncapped persisted data. This can lead to auto-drain dispatching incorrect messages and leaves older, undrainable messages in persistence and the UI.
<!-- DESCRIPTION END -->

<!-- BUGBOT_BUG_ID: c76cc5f6-52df-4e72-8076-e2535882a772 -->

<!-- LOCATIONS START
apps/server/src/orchestration/projector.ts#L447-L470
LOCATIONS END -->
<div><a href="https://cursor.com/open?link=eyJ2ZXJzaW9uIjoxLCJ0eXBlIjoiQlVHQk9UX0ZJWF9JTl9DVVJTT1IiLCJkYXRhIjp7InJlZGlzS2V5IjoiYnVnYm90OjMyZTlkNmI0LWRmZTAtNDQyMC04NTVhLTQ3ODNiNzA5Zjg5ZSIsImVuY3J5cHRpb25LZXkiOiJXWEMxaUNBQ1VMX2l6SlJJMG5WSldnYVRndGx3UVBPUWhnMktmUkd3ZURRIiwiYnJhbmNoIjoidDNjb2RlL3F1ZXVlLXN0ZWVyLWZlYXR1cmUiLCJyZXBvT3duZXIiOiJwaW5nZG90Z2ciLCJyZXBvTmFtZSI6InQzY29kZSJ9fQ" target="_blank" rel="noopener noreferrer"><picture><source media="(prefers-color-scheme: dark)" srcset="https://cursor.com/assets/images/fix-in-cursor-dark.png"><source media="(prefers-color-scheme: light)" srcset="https://cursor.com/assets/images/fix-in-cursor-light.png"><img alt="Fix in Cursor" width="115" height="28" src="https://cursor.com/assets/images/fix-in-cursor-dark.png"></picture></a>&nbsp;<a href="https://cursor.com/agents?link=eyJ2ZXJzaW9uIjoxLCJ0eXBlIjoiQlVHQk9UX0ZJWF9JTl9XRUIiLCJkYXRhIjp7InJlZGlzS2V5IjoiYnVnYm90OjMyZTlkNmI0LWRmZTAtNDQyMC04NTVhLTQ3ODNiNzA5Zjg5ZSIsImVuY3J5cHRpb25LZXkiOiJXWEMxaUNBQ1VMX2l6SlJJMG5WSldnYVRndGx3UVBPUWhnMktmUkd3ZURRIiwiYnJhbmNoIjoidDNjb2RlL3F1ZXVlLXN0ZWVyLWZlYXR1cmUiLCJyZXBvT3duZXIiOiJwaW5nZG90Z2ciLCJyZXBvTmFtZSI6InQzY29kZSIsInByTnVtYmVyIjo0MjQ1LCJjb21taXRTaGEiOiIyOTlkOTYxZjY3MDMzN2U2YzEwZDAyMGE0ODkzODBkZGNiNjlhZDFlIiwicHJvdmlkZXIiOiJnaXRodWIifX0" target="_blank" rel="noopener noreferrer"><picture><source media="(prefers-color-scheme: dark)" srcset="https://cursor.com/assets/images/fix-in-web-dark.png"><source media="(prefers-color-scheme: light)" srcset="https://cursor.com/assets/images/fix-in-web-light.png"><img alt="Fix in Web" width="99" height="28" src="https://cursor.com/assets/images/fix-in-web-dark.png"></picture></a></div>


<sup>Reviewed by [Cursor Bugbot](https://cursor.com/bugbot) for commit 299d961f670337e6c10d020a489380ddcb69ad1e. Configure [here](https://www.cursor.com/dashboard/bugbot).</sup>
