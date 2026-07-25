### Composer acks unrelated queue changes

**Medium Severity**

<!-- DESCRIPTION START -->
While a turn is running, local “Sending” clears when either the latest timeline user message id or the latest queued message id changes. Steering or removing another queued item can satisfy that check before the in-flight send is queued, so the composer stops showing Sending even though that message was never acknowledged.
<!-- DESCRIPTION END -->

<!-- BUGBOT_BUG_ID: d228c05b-14a4-4184-81ea-44242ad98ce2 -->

<!-- LOCATIONS START
apps/web/src/components/ChatView.logic.ts#L443-L460
LOCATIONS END -->
<div><a href="https://cursor.com/open?link=eyJ2ZXJzaW9uIjoxLCJ0eXBlIjoiQlVHQk9UX0ZJWF9JTl9DVVJTT1IiLCJkYXRhIjp7InJlZGlzS2V5IjoiYnVnYm90OjY2N2E3NzczLTU2OGItNDU5ZC1hZWRjLTFjMmYyOWIzNWNjYiIsImVuY3J5cHRpb25LZXkiOiJuYW1GZVFVMGs0bDM1TzQtQmVNNXBXcEhEbF8wZHp5YUtmZXdxajU5MENJIiwiYnJhbmNoIjoidDNjb2RlL3F1ZXVlLXN0ZWVyLWZlYXR1cmUiLCJyZXBvT3duZXIiOiJwaW5nZG90Z2ciLCJyZXBvTmFtZSI6InQzY29kZSJ9fQ" target="_blank" rel="noopener noreferrer"><picture><source media="(prefers-color-scheme: dark)" srcset="https://cursor.com/assets/images/fix-in-cursor-dark.png"><source media="(prefers-color-scheme: light)" srcset="https://cursor.com/assets/images/fix-in-cursor-light.png"><img alt="Fix in Cursor" width="115" height="28" src="https://cursor.com/assets/images/fix-in-cursor-dark.png"></picture></a>&nbsp;<a href="https://cursor.com/agents?link=eyJ2ZXJzaW9uIjoxLCJ0eXBlIjoiQlVHQk9UX0ZJWF9JTl9XRUIiLCJkYXRhIjp7InJlZGlzS2V5IjoiYnVnYm90OjY2N2E3NzczLTU2OGItNDU5ZC1hZWRjLTFjMmYyOWIzNWNjYiIsImVuY3J5cHRpb25LZXkiOiJuYW1GZVFVMGs0bDM1TzQtQmVNNXBXcEhEbF8wZHp5YUtmZXdxajU5MENJIiwiYnJhbmNoIjoidDNjb2RlL3F1ZXVlLXN0ZWVyLWZlYXR1cmUiLCJyZXBvT3duZXIiOiJwaW5nZG90Z2ciLCJyZXBvTmFtZSI6InQzY29kZSIsInByTnVtYmVyIjo0MjQ1LCJjb21taXRTaGEiOiJmMjIyODM0ZTg0N2I2NmY4Mzg5YTliMzVlMWJkMGNlMWRiYjEwYmE4IiwicHJvdmlkZXIiOiJnaXRodWIifX0" target="_blank" rel="noopener noreferrer"><picture><source media="(prefers-color-scheme: dark)" srcset="https://cursor.com/assets/images/fix-in-web-dark.png"><source media="(prefers-color-scheme: light)" srcset="https://cursor.com/assets/images/fix-in-web-light.png"><img alt="Fix in Web" width="99" height="28" src="https://cursor.com/assets/images/fix-in-web-dark.png"></picture></a></div>


<sup>Reviewed by [Cursor Bugbot](https://cursor.com/bugbot) for commit f222834e847b66f8389a9b35e1bd0ce1dbb10ba8. Configure [here](https://www.cursor.com/dashboard/bugbot).</sup>
