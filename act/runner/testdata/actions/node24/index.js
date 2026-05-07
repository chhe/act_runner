import {getInput, setOutput, setFailed} from '@actions/core';
import {context} from '@actions/github';

try {
  const nameToGreet = getInput('who-to-greet');
  console.log(`Hello ${nameToGreet}!`);
  setOutput('time', (new Date()).toTimeString());
  console.log(`The event payload: ${JSON.stringify(context.payload, undefined, 2)}`);
} catch (error) {
  setFailed(error.message);
}
