// Copyright 2019 The Cockroach Authors.
//
// Licensed under the Apache License, Version 2.0 (the "License");
// you may not use this file except in compliance with the License.
// You may obtain a copy of the License at
//
//     http://www.apache.org/licenses/LICENSE-2.0
//
// Unless required by applicable law or agreed to in writing, software
// distributed under the License is distributed on an "AS IS" BASIS,
// WITHOUT WARRANTIES OR CONDITIONS OF ANY KIND, either express or
// implied. See the License for the specific language governing
// permissions and limitations under the License.

const fs                 = require('fs');
const assert             = require('assert');
const rejectsWithPGError = require('./rejects-with-pg-error');
const client             = require('./client');

// We orchestrate a failure here to ensure that a failing test actually causes
// the docker build to fail.
if (process.env.SHOULD_FAIL) {
  describe('failure smoke test', () => {
    it('causes the docker build to fail on a test failure', () => {
      assert.fail();
    });
  });
}

describe('select', () => {
  it('lets you select values', () => {
    return client.query("SELECT 1 as first, 2+$1 as second, ARRAY['\"','',''] as third", [3])
      .then(results => {
        assert.deepEqual(results.rows, [{
          first: 1,
          second: 5,
          third: ['"', '', '']
        }]);
      });
  });
});

describe('error cases', () => {
  const cases = [{
    name: 'not enough params',
    query: { text: 'SELECT 3', values: ['foo'] },
    msg: "expected 0 arguments, got 1",
    code: '08P01',
  }, {
    name: 'invalid utf8',
    query: { text: 'SELECT $1::STRING', values: [new Buffer([167])] },
    msg: "invalid UTF-8 sequence",
    code: '22021',
  }];

  cases.forEach(({ name, query, msg, code }) => {
    it(`${name} # ${query.text}`, () => {
      return rejectsWithPGError(client.query(query), { msg, code });
    });
  });
});

const NUMERIC_TYPES = ['INT', 'FLOAT', 'DECIMAL'];

describe('arrays', () => {
  it('can be selected', () => {
    return client.query('SELECT ARRAY[1, 2, 3] a')
      .then(results => {
        assert.deepEqual([1, 2, 3], results.rows[0].a);
      });
  });

  NUMERIC_TYPES.forEach(t => {
    it(`can be passed as a placeholder for a ${t}[]`, () => {
      return client.query(`SELECT $1:::${t}[] a`, [[1, 2, 3]])
        .then(results => {
          assert.deepEqual([1, 2, 3], results.rows[0].a);
        });
    });
  });
});

describe('regression tests', () => {
  it('allows you to switch between format modes for arrays', () => {
    return client.query({
            text: 'SELECT $1:::int[] as b',
            values: [[1, 2, 8]],
            binary: false,
          }).then(r => {
            return client.query({
              text: 'SELECT $1:::int[] a',
              values: [[4, 5, 6]],
              binary: true,
            });
          }).then(results => {
            assert.deepEqual([4, 5, 6], results.rows[0].a);
          });
  });
})
