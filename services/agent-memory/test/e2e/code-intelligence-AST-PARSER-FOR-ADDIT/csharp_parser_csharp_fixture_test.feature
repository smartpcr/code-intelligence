@story-code-intelligence:AST-PARSER-FOR-ADDIT @phase-csharp-parser @stage-csharp-fixture-test @setup-inline
Feature: C# fixture test — node and edge emission

  The csharp-fixture-test stage validates end-to-end parsing of a
  representative C# fixture through the dispatcher's EmitFile path,
  verifying node/edge counts, base-list partition (Extends vs
  Implements), and the partial-class LangMeta flag.

  Scenario: C# fixture node and edge count
    Given the C# fixture source:
      """
      using System;

      interface IGreeter {
          void Greet();
      }

      class BaseService {
          void Log() { }
      }

      class MyService : BaseService, IGreeter {
          void Greet() { }
          void Process() { Log(); }
      }
      """
    When EmitFile runs under CGO on
    Then 3 class nodes are emitted
    And 4 method nodes are emitted
    And 1 package node is emitted
    And 1 extends edge is emitted
    And 1 implements edge is emitted
    And 1 static_calls edge is emitted
    And 1 imports edge is emitted

  Scenario Outline: Base-list partition decision matrix
    Given C# partition source "<source>"
    When the partition source is parsed with the C# parser
    Then class "<class>" Extends list is "<extends>" and Implements list is "<implements>"

    Examples:
      | source                                                                      | class | extends | implements |
      | class B {} class A : B {}                                                   | A     | B       |            |
      | interface IFoo {} class A : IFoo {}                                         | A     |         | IFoo       |
      | class B {} interface IFoo {} class A : B, IFoo {}                           | A     | B       | IFoo       |
      | class B {} interface IFoo {} interface IBar {} class A : B, IFoo, IBar {}   | A     | B       | IFoo,IBar  |
      | interface IFoo {} struct S : IFoo {}                                        | S     |         | IFoo       |
      | interface IBar {} interface IFoo : IBar {}                                  | IFoo  | IBar    |            |

  Scenario: Partial class flag
    Given C# partial class source:
      """
      partial class Foo {}
      """
    When the partial source is parsed with the C# parser
    Then the class "Foo" has LangMeta partial equal to true